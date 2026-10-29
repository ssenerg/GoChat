package push

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/mention"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/hub"
)

// Posted is a message a channel's actor posted. It shares its fields with
// the actor's events: read only.
type Posted struct {
	Message      *pb.Message
	TeamID       gocql.UUID
	Conversation bool
	// ChannelName is a channel's name, empty for a conversation.
	ChannelName string
	// Members are the channel's people, bots left out.
	Members hub.Members
	// Participants are those a reply's thread tells.
	Participants []string
}

// A message's pushes go out off the workers, so a slow FCM or APNs holds
// up no registration: sends bounds the people pushed to at once, over all
// messages, and backlog the messages waiting, past which one's pushes are
// dropped.
const (
	sends   = 64
	backlog = 1024
)

type fanOut struct {
	slots   chan struct{}
	waiting atomic.Int32
	running sync.WaitGroup
}

func newFanOut() *fanOut { return &fanOut{slots: make(chan struct{}, sends)} }

// posted pushes a message to whom it calls for, as the apps' notices do:
// the others of a conversation, the people of a reply's thread and those
// it mentions. Only people with no connection open get it; @here calls for
// those online, so it pushes to no one.
func (w *worker) posted(msg Posted) {
	// Each step has its own deadline, so a slow one costs the next none.
	targets := w.targets(msg)
	if len(targets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	away, err := actors.Ask[[]string](ctx, w.Engine, w.Hub, hub.Disconnected{UserIDs: targets})
	cancel()
	if err != nil {
		w.Logger.Warn("failed to find who to push to", zap.Error(err))
		return
	}
	away = w.unblocked(msg.Message.GetAuthor().GetId(), away)
	if len(away) == 0 {
		return
	}
	n := w.notice(msg)
	channelID, _ := ids.Parse(msg.Message.GetChannelId())
	f := w.fan
	if f.waiting.Add(1) > backlog {
		f.waiting.Add(-1)
		w.Logger.Warn("pushes fell behind: a message's were dropped", zap.String("message", msg.Message.GetId()))
		return
	}
	f.running.Add(1)
	go func() {
		defer f.running.Done()
		defer f.waiting.Add(-1)
		var each sync.WaitGroup
		for _, raw := range away {
			userID, ok := ids.Parse(raw)
			if !ok {
				continue
			}
			f.slots <- struct{}{}
			each.Go(func() {
				defer func() { <-f.slots }()
				w.reach(userID, channelID, n)
			})
		}
		each.Wait()
	}()
}

// targets are the people msg calls for, the author left out.
func (w *worker) targets(msg Posted) []string {
	m := msg.Message
	author := m.GetAuthor().GetId()
	set := map[string]bool{}
	add := func(id string) {
		if _, ok := msg.Members[id]; ok && id != author {
			set[id] = true
		}
	}
	found := mention.Parse(m.GetContent())
	if msg.Conversation || found.Channel {
		for id := range msg.Members {
			add(id)
		}
	}
	if m.GetRootId() != "" {
		for _, id := range msg.Participants {
			add(id)
		}
	}
	if len(found.Usernames) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
		defer cancel()
		for _, name := range found.Usernames {
			id, err := w.Repo.GetUserIDByUsername(ctx, name)
			if err == nil {
				add(id.String())
			} else if !ierrors.Is(err, ierrors.CodeNotFound) {
				w.Logger.Warn("failed to find a mentioned person", zap.Error(err))
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// unblocked leaves out those of userIDs who blocked author. Unread blocks
// push nothing.
func (w *worker) unblocked(author string, userIDs []string) []string {
	aid, ok := ids.Parse(author)
	if w.Blocks == nil || !ok {
		return userIDs
	}
	uids := make([]gocql.UUID, 0, len(userIDs))
	for _, id := range userIDs {
		if uid, ok := ids.Parse(id); ok {
			uids = append(uids, uid)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	defer cancel()
	blocked, err := w.Blocks.Among(ctx, aid, uids)
	if err != nil {
		w.Logger.Warn("failed to read who blocked a message's author", zap.Error(err))
		return nil
	}
	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		if !blocked[uid] {
			out = append(out, uid.String())
		}
	}
	return out
}

// reach pushes n to userID's devices, unless the channel is muted or they
// do not want to be disturbed.
func (w *worker) reach(userID, channelID gocql.UUID, n *Notice) {
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	defer cancel()
	devices, err := w.Repo.ListPushDevices(ctx, userID)
	if err != nil {
		w.Logger.Warn("failed to list a person's devices", zap.Error(err))
		return
	}
	if len(devices) == 0 {
		return
	}
	muted, err := w.Repo.IsMuted(ctx, userID, channelID)
	if err != nil || muted {
		return
	}
	u, err := w.Users.Get(ctx, userID)
	if err != nil || u.Deactivated || disturbing(u, time.Now()) {
		return
	}
	for _, d := range devices {
		w.deliver(d, n)
	}
}

// deliver pushes n to one device, on deadlines of its own: one out of
// reach costs the person's others nothing.
func (w *worker) deliver(d *models.PushDevice, n *Notice) {
	p := w.Providers[d.Platform]
	if p == nil || !w.alive(d) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.SendTimeout)
	err := p.Send(ctx, d, n)
	cancel()
	switch {
	case errors.Is(err, ErrDeadToken):
		// The session may have a new token by now, which stays.
		ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
		defer cancel()
		if err := w.Repo.ForgetPushToken(ctx, d); err != nil {
			w.Logger.Warn("failed to forget a dead device token", zap.Error(err))
		}
	case err != nil:
		w.Logger.Warn("push failed", zap.String("platform", d.Platform), zap.Error(err))
	}
}

// disturbing reports whether u does not want to be disturbed at now.
func disturbing(u *models.User, now time.Time) bool {
	return u.Status == "dnd" && (u.DNDUntil.Unix() <= 0 || now.Before(u.DNDUntil))
}

// alive reports whether d's session may still be used, forgetting d when
// it may not: it ended, or went unused past the idle timeout.
func (w *worker) alive(d *models.PushDevice) bool {
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	defer cancel()
	s, err := w.Repo.GetSession(ctx, d.SessionHash)
	if err != nil && !ierrors.Is(err, ierrors.CodeNotFound) {
		w.Logger.Warn("failed to read a device's session", zap.Error(err))
		return false
	}
	now := time.Now()
	ok := err == nil && s.UserID == d.UserID && now.Before(s.ExpiresAt) &&
		(w.IdleTimeout <= 0 || now.Sub(lastActive(s)) <= w.IdleTimeout)
	if !ok {
		if err := w.Repo.DeletePushDevice(ctx, d); err != nil {
			w.Logger.Warn("failed to forget the device of an ended session", zap.Error(err))
		}
	}
	return ok
}

func lastActive(s *models.Session) time.Time {
	if s.LastSeen.After(s.CreatedAt) {
		return s.LastSeen
	}
	return s.CreatedAt
}

// notice is what msg's push shows: who wrote, where, and an excerpt.
func (w *worker) notice(msg Posted) *Notice {
	m := msg.Message
	title := displayName(m.GetAuthor())
	where := ""
	if !msg.Conversation && msg.ChannelName != "" {
		where = " in #" + msg.ChannelName
	}
	if m.GetRootId() != "" {
		title += " replied" + where
	} else {
		title += where
	}
	body := ""
	if w.Cfg.Excerpts {
		body = excerpt(m)
	}
	if body == "" {
		body = "New message"
	}
	team := ""
	if !ids.IsZero(msg.TeamID) {
		team = msg.TeamID.String()
	}
	data := map[string]string{"channel_id": m.GetChannelId(), "message_id": m.GetId(), "team_id": team}
	if m.GetRootId() != "" {
		data["root_id"] = m.GetRootId()
	}
	return &Notice{Title: title, Body: body, Thread: m.GetChannelId(), Data: data}
}

func displayName(u *pb.User) string {
	switch {
	case u == nil:
		return "Someone"
	case u.DisplayName != "":
		return u.DisplayName
	}
	return u.Username
}

const excerptRunes = 140

// excerpt is a message's text, short and on one line, as the web's
// notices cut it.
func excerpt(m *pb.Message) string {
	text := strings.Join(strings.Fields(m.GetContent()), " ")
	if r := []rune(text); len(r) > excerptRunes {
		text = string(r[:excerptRunes]) + "…"
	}
	if text == "" {
		if n := len(m.GetAttachments()); n == 1 && m.Attachments[0].GetVoice() {
			text = "Sent a voice message"
		} else if n == 1 {
			text = "Sent a file"
		} else if n > 1 {
			text = fmt.Sprintf("Sent %d files", n)
		}
	}
	return text
}
