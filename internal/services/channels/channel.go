package channels

import (
	"context"
	"slices"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/emoji"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/mention"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/validate"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/index"
	"github.com/ssenerg/gochat/internal/services/updates"
)

// DefaultChannel is the channel every team member stays in.
const DefaultChannel = "town-square"

type idleCheck struct{}

type channel struct {
	*Deps
	id      gocql.UUID
	ch      *models.Channel
	members map[gocql.UUID]models.ChannelRole
	// cleared holds up to which message each person of a direct
	// conversation cleared it.
	cleared map[gocql.UUID]gocql.UUID
	// recipients is who the channel's events go to.
	recipients hub.Members
	bucket     int32
	hasBucket  bool
	typing     map[gocql.UUID]time.Time
	posters    map[gocql.UUID]*rate.Limiter
	pins       int
	active     time.Time
	loaded     bool
	loadErr    error
	asked      bool
	handled    uint64
	repeater   *actor.SendRepeater
	// pts numbers the channel's latest update; stamp is when its state was
	// last written, so a later number always wins.
	pts   int64
	stamp int64
	bots  map[gocql.UUID]*botMember
}

func newChannel(d *Deps, id gocql.UUID) actor.Producer {
	return func() actor.Receiver {
		return &channel{Deps: d, id: id}
	}
}

func (a *channel) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), a.Cfg.OpTimeout)
}

func (a *channel) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		// Loading waits for the first request: Started runs on the
		// manager's goroutine.
		a.active = time.Now()
		r := c.SendRepeat(c.PID(), idleCheck{}, a.Cfg.IdleTimeout/2)
		a.repeater = &r
		return
	case actor.Stopped:
		if a.repeater != nil {
			a.repeater.Stop()
		}
		return
	case actor.Initialized:
		return
	case idleCheck:
		if time.Since(a.active) >= a.Cfg.IdleTimeout {
			a.passivate(c)
		}
		return
	case Request:
		a.active = time.Now()
		a.handled++
		a.asked = false
		if !a.loaded {
			a.loaded = true
			a.load()
		}
		if a.loadErr != nil {
			actors.Reply(c, a.loadErr)
			a.passivate(c)
			return
		}
		a.handle(c, msg)
	}
}

func (a *channel) passivate(c *actor.Context) {
	if !a.asked {
		a.asked = true
		c.Send(c.Parent(), passivate{id: a.id, pid: c.PID(), handled: a.handled})
	}
}

func (a *channel) load() {
	ctx, cancel := a.ctx()
	defer cancel()
	a.typing = make(map[gocql.UUID]time.Time)
	a.posters = make(map[gocql.UUID]*rate.Limiter)
	ch, err := a.Repo.GetChannel(ctx, a.id)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			a.loadErr = ierrors.NotFound("channel not found")
		} else {
			a.Logger.Error("failed to load channel", zap.String("channel", a.id.String()), zap.Error(err))
			a.loadErr = ierrors.Unavailable("channel is unavailable, try again")
		}
		return
	}
	members, err := a.Repo.ListChannelMembers(ctx, a.id)
	if err != nil {
		a.Logger.Error("failed to load channel members", zap.String("channel", a.id.String()), zap.Error(err))
		a.loadErr = ierrors.Unavailable("channel is unavailable, try again")
		return
	}
	bucket, has, err := a.Repo.LastMessageBucket(ctx, a.id)
	if err != nil {
		a.Logger.Error("failed to load channel bucket", zap.String("channel", a.id.String()), zap.Error(err))
		a.loadErr = ierrors.Unavailable("channel is unavailable, try again")
		return
	}
	pins, err := a.Repo.ListPins(ctx, a.id, maxPins)
	if err != nil {
		a.Logger.Error("failed to load channel pins", zap.String("channel", a.id.String()), zap.Error(err))
		a.loadErr = ierrors.Unavailable("channel is unavailable, try again")
		return
	}
	pts, err := a.Repo.ChannelPts(ctx, a.id)
	if err != nil {
		a.Logger.Error("failed to load channel updates", zap.String("channel", a.id.String()), zap.Error(err))
		a.loadErr = ierrors.Unavailable("channel is unavailable, try again")
		return
	}
	a.pts = pts
	a.pins = len(pins)
	a.ch = ch
	a.bucket, a.hasBucket = bucket, has
	a.members = make(map[gocql.UUID]models.ChannelRole, len(members))
	for _, m := range members {
		a.members[m.UserID] = m.Role
	}
	if err := a.loadBots(ctx, members); err != nil {
		a.Logger.Error("failed to load a channel's bots", zap.String("channel", a.id.String()), zap.Error(err))
		a.loadErr = ierrors.Unavailable("channel is unavailable, try again")
		return
	}
	a.cleared = make(map[gocql.UUID]gocql.UUID)
	if ch.Kind == models.ChannelDirect {
		for _, m := range members {
			id, err := a.Repo.GetCleared(ctx, m.UserID, a.id)
			if err != nil {
				a.Logger.Error("failed to load a conversation's clears", zap.String("channel", a.id.String()), zap.Error(err))
				a.loadErr = ierrors.Unavailable("channel is unavailable, try again")
				return
			}
			if !ids.IsZero(id) {
				a.cleared[m.UserID] = id
			}
		}
	}
	a.snapshot()
}

// snapshot rebuilds the recipients, the members who are not bots. Deliver
// messages share them, so they are replaced, never modified.
func (a *channel) snapshot() {
	out := make(hub.Members, len(a.members))
	for id := range a.members {
		if _, bot := a.bots[id]; !bot {
			out[id.String()] = struct{}{}
		}
	}
	a.recipients = out
}

// deliver numbers ev in one user's updates and sends it to them.
func (a *channel) deliver(userID gocql.UUID, ev *pb.Event) {
	if a.Updates == nil {
		a.Engine.Send(a.Hub, hub.Deliver{UserIDs: []string{userID.String()}, Event: ev})
		return
	}
	a.Engine.Send(a.Updates, updates.Push{UserIDs: []gocql.UUID{userID}, Event: ev})
}

// broadcast numbers ev in the channel's updates and sends it to every
// member, and to the bots that see it: about names the message it is
// about, and that message's thread root when it is a reply.
func (a *channel) broadcast(ev *pb.Event, about ...*models.Message) {
	a.number(ev)
	if len(a.recipients) > 0 {
		a.Engine.Send(a.Hub, hub.Deliver{Members: a.recipients, Event: ev})
	}
	var m, root *models.Message
	if len(about) > 0 {
		m = about[0]
	}
	if len(about) > 1 {
		root = about[1]
	}
	a.toBots(ev, m, root)
}

// number gives ev the channel's next number and logs it, before anyone
// sees it, so a difference read after it was seen finds it.
func (a *channel) number(ev *pb.Event) {
	a.pts++
	ev.Pts, ev.PtsCount, ev.PtsChannelId = a.pts, 1, a.id.String()
	if cu := ev.GetChannelUpdated(); cu != nil {
		cu.Channel.Pts = a.pts
	}
	e := updates.Entry(ev)
	e.Pts, e.ChannelID = a.pts, gocql.UUID{}
	a.stamp = max(time.Now().UnixMicro(), a.stamp+1)
	ctx, cancel := a.ctx()
	defer cancel()
	if err := a.Repo.AppendChannelUpdate(ctx, a.id, e, a.stamp, a.UpdatesCfg.Retention); err != nil {
		a.Logger.Warn("failed to log a channel update", zap.String("channel", a.id.String()), zap.Error(err))
	}
}

func (a *channel) handle(c *actor.Context, req Request) {
	var (
		out any
		err error
	)
	if err := a.voiceRefuses(req); err != nil {
		actors.Reply(c, err)
		return
	}
	switch msg := req.(type) {
	case VoiceAccess:
		out, err = a.voiceAccess(msg.P)
	case Access:
		ch := *a.ch
		out = &AccessResult{Channel: &ch, Role: a.members[msg.P.UserID]}
	case View:
		out, err = a.view(msg.P)
	case Post:
		out, err = a.post(msg)
	case Edit:
		out, err = a.edit(msg)
	case Delete:
		out, err = a.delete(msg)
	case History:
		err = a.history(c, msg)
	case GetMessage:
		err = a.getMessage(c, msg)
	case Thread:
		err = a.thread(c, msg)
	case Typing:
		out, err = a.typingEvent(msg)
	case React:
		out, err = a.react(msg)
	case Mute:
		out, err = a.mute(msg)
	case Settings:
		out, err = a.settings(msg)
	case Privacy:
		out, err = a.privacy(msg)
	case Archive:
		out, err = a.archive(msg)
	case ThreadRead:
		err = a.threadRead(c, msg)
	case Pin:
		out, err = a.pin(msg)
	case Pins:
		err = a.listPins(c, msg)
	case Save:
		out, err = a.save(msg)
	case Follow:
		out, err = a.follow(msg)
	case SetVoice:
		out, err = a.setVoice(msg)
	case MarkRead:
		err = a.markRead(c, msg)
	case Join:
		out, err = a.join(msg)
	case Leave:
		out, err = a.leave(msg)
	case AddMember:
		out, err = a.addMember(msg)
	case RemoveMember:
		out, err = a.removeMember(msg)
	case Members:
		err = a.listMembers(c, msg)
	case Rejoin:
		out, err = a.rejoin(msg)
	case Move:
		out, err = a.move(msg)
	case DeleteConversation:
		out, err = a.deleteConversation(msg)
	case Difference:
		err = a.difference(c, msg)
	case Press:
		out, err = a.press(msg)
	case BotMembers:
		out, err = a.botIDs(msg)
	default:
		return
	}
	if err != nil {
		if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
			a.Logger.Error("channel request failed", append(e.Fields(), zap.String("channel", a.id.String()))...)
		}
		actors.Reply(c, err)
		return
	}
	if out != nil {
		actors.Reply(c, out)
	}
}

// voiceRefuses refuses in a voice channel what only text channels do.
func (a *channel) voiceRefuses(req Request) error {
	if a.ch.Kind != models.ChannelVoice {
		return nil
	}
	switch req.(type) {
	case Post, Edit, Delete, History, GetMessage, Thread, ThreadRead, Typing, React, Pin, Pins, Save, Follow, Mute, MarkRead, SetVoice, Difference, Press:
		return ierrors.FailedPrecondition("voice channels carry no messages")
	case Join, AddMember:
		return ierrors.FailedPrecondition("everyone in the team may talk in a voice channel: join the talk instead")
	case Settings, Privacy, Archive:
		return ierrors.FailedPrecondition("voice channels have no settings")
	}
	return nil
}

// voiceAccess tells what the caller may do in the talk of a voice channel:
// members of its team connect, its moderators disconnect others.
func (a *channel) voiceAccess(p *models.Principal) (*VoiceAccessResult, error) {
	if a.ch.Kind != models.ChannelVoice {
		return nil, ierrors.NotFound("voice channel not found")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	tr, err := a.teamRole(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if tr == "" && p.Role != models.SystemRoleAdmin {
		return nil, ierrors.NotFound("voice channel not found")
	}
	ch := *a.ch
	return &VoiceAccessResult{
		Channel:     &ch,
		CanConnect:  a.Authz.Can(authz.ObjVoice, authz.ActConnect, string(tr), string(p.Role)),
		CanModerate: a.Authz.Can(authz.ObjVoice, authz.ActMove, string(a.members[p.UserID]), string(tr), string(p.Role)),
	}, nil
}

// open reports whether the team's members may see the channel without
// belonging to it.
func (a *channel) open() bool {
	return a.ch.Kind == models.ChannelPublic || a.ch.Kind == models.ChannelVoice
}

func (a *channel) member(p *models.Principal) (models.ChannelRole, error) {
	role, ok := a.members[p.UserID]
	if !ok {
		if a.ch.Kind == models.ChannelPublic {
			return "", ierrors.New(ierrors.CodePermissionDenied, "join the channel first")
		}
		return "", ierrors.NotFound("channel not found")
	}
	return role, nil
}

func (a *channel) teamRole(ctx context.Context, userID gocql.UUID) (models.TeamRole, error) {
	if a.ch.Kind.Conversation() {
		return "", nil
	}
	tm, err := a.Repo.GetTeamMember(ctx, a.ch.TeamID, userID)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return "", nil
		}
		return "", err
	}
	return tm.Role, nil
}

func (a *channel) view(p *models.Principal) (*pb.Channel, error) {
	role, ok := a.members[p.UserID]
	if !ok && !a.open() {
		return nil, ierrors.NotFound("channel not found")
	}
	if !ok {
		ctx, cancel := a.ctx()
		defer cancel()
		tr, err := a.teamRole(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		if tr == "" && p.Role != models.SystemRoleAdmin {
			return nil, ierrors.NotFound("channel not found")
		}
	}
	out, err := a.channelFor(p.UserID, role)
	if err != nil || !ok {
		return out, err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	if out.Muted, err = a.Repo.IsMuted(ctx, p.UserID, a.id); err != nil {
		a.Logger.Warn("failed to read a channel's mute", zap.String("channel", a.id.String()), zap.Error(err))
	}
	return out, nil
}

func (a *channel) mute(msg Mute) (Done, error) {
	if _, err := a.member(msg.P); err != nil {
		return Done{}, err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	if err := a.Repo.SetMuted(ctx, msg.P.UserID, a.id, msg.Muted); err != nil {
		return Done{}, err
	}
	a.deliver(msg.P.UserID, &pb.Event{Payload: &pb.Event_ChannelMuted{ChannelMuted: &pb.ChannelMuted{
		ChannelId: a.id.String(), Muted: msg.Muted,
	}}})
	return Done{}, nil
}

// channelFor renders the channel as userID sees it.
func (a *channel) channelFor(userID gocql.UUID, role models.ChannelRole) (*pb.Channel, error) {
	if !a.ch.Kind.Conversation() {
		out := convert.Channel(a.ch, role, gocql.UUID{}, nil)
		out.PinnedCount = int32(a.pins) //nolint:gosec // at most maxPins
		out.Pts = a.pts
		return out, nil
	}
	ctx, cancel := a.ctx()
	defer cancel()
	people, err := a.Users.GetMany(ctx, convert.PeerIDs(a.ch, userID))
	if err != nil {
		return nil, err
	}
	out := convert.Conversation(a.ch, role, gocql.UUID{}, userID, people)
	out.PinnedCount = int32(a.pins) //nolint:gosec // at most maxPins
	out.Pts = a.pts
	return out, nil
}

func (a *channel) poster(userID gocql.UUID) *rate.Limiter {
	l := a.posters[userID]
	if l == nil {
		l = rate.NewLimiter(rate.Limit(a.Cfg.PostRate), a.Cfg.PostBurst)
		a.posters[userID] = l
	}
	return l
}

func parseIDs(raw []string) ([]gocql.UUID, error) {
	seen := make(map[gocql.UUID]bool, len(raw))
	out := make([]gocql.UUID, 0, len(raw))
	for _, s := range raw {
		id, ok := ids.Parse(s)
		if !ok {
			return nil, ierrors.InvalidArgument("invalid attachment id")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func (a *channel) post(msg Post) (*pb.Message, error) {
	role, err := a.member(msg.P)
	if err != nil {
		return nil, err
	}
	if !a.Authz.Can(authz.ObjMessage, authz.ActCreate, string(role), string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	if a.archived() {
		return nil, errArchived
	}
	if !a.poster(msg.P.UserID).Allow() {
		return nil, ierrors.ResourceExhausted("you are posting too fast")
	}
	content, err := validate.Text("message", msg.Content, a.Cfg.MaxMessageLength)
	if err != nil {
		return nil, err
	}
	if len(msg.AttachmentIDs) > a.Cfg.MaxAttachments {
		return nil, ierrors.InvalidArgument("too many attachments")
	}
	attIDs, err := parseIDs(msg.AttachmentIDs)
	if err != nil {
		return nil, err
	}
	if content == "" && len(attIDs) == 0 {
		return nil, ierrors.InvalidArgument("message is empty")
	}
	markup, err := markupOf(msg.P, msg.Markup)
	if err != nil {
		return nil, err
	}

	ctx, cancel := a.ctx()
	defer cancel()
	// Fetched first: a failure after the write would report a saved
	// message as lost, and a retry would post it twice.
	author, err := a.Users.Get(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	var root *models.Message
	if msg.RootID != "" {
		if root, err = a.messageOf(ctx, msg.RootID, ""); err != nil {
			return nil, err
		}
	}
	atts, err := a.ownAttachments(ctx, msg.P.UserID, attIDs)
	if err != nil {
		return nil, err
	}
	if err := a.checkVoice(atts); err != nil {
		return nil, err
	}
	id := ids.NewTime()
	m := &models.Message{
		ChannelID:   a.id,
		Bucket:      ids.BucketOf(id),
		ID:          id,
		UserID:      msg.P.UserID,
		Content:     content,
		Attachments: attIDs,
		Markup:      markup,
	}
	for i, att := range atts {
		ok, err := a.Repo.ClaimAttachment(ctx, att, id)
		if err == nil && !ok {
			err = ierrors.InvalidArgument("an attachment is no longer available")
		}
		if err != nil {
			a.unclaim(atts[:i], id)
			return nil, err
		}
	}
	if root != nil {
		return a.reply(ctx, root, m, author, atts)
	}
	newBucket := !a.hasBucket || m.Bucket != a.bucket
	if err := a.Repo.InsertMessage(ctx, m, newBucket); err != nil {
		// A message that may still land keeps its attachments.
		if !scylla.Uncertain(err) {
			a.unclaim(atts, id)
		}
		return nil, err
	}
	a.bucket, a.hasBucket = m.Bucket, true
	prev := a.ch.LastMessageID
	a.ch.LastMessageID = id
	if err := a.Repo.SetLastMessage(ctx, a.id, id); err != nil {
		a.Logger.Warn("failed to record the last message", zap.String("channel", a.id.String()), zap.Error(err))
	}
	a.reappear(prev)
	if err := a.Repo.SetLastRead(ctx, msg.P.UserID, a.id, id); err != nil {
		a.Logger.Warn("failed to mark own message read", zap.Error(err))
	}
	delete(a.typing, msg.P.UserID)

	out := convert.Message(m, convert.PublicUser(author), atts)
	a.broadcast(&pb.Event{Payload: &pb.Event_MessagePosted{MessagePosted: a.posted(out, nil)}}, m)
	a.recordMentions(m)
	a.file(index.Add{Doc: a.doc(m)})
	return out, nil
}

func (a *channel) doc(m *models.Message) index.Doc {
	members := make([]gocql.UUID, 0, len(a.members))
	for id := range a.members {
		members = append(members, id)
	}
	return index.Doc{
		Scopes:  index.Scopes(a.ch, members),
		Posting: scylla.Posting{MessageID: m.ID, ChannelID: a.id, RootID: m.RootID, UserID: m.UserID},
		Content: m.Content,
	}
}

// file hands a change of a message to the search index.
func (a *channel) file(msg any) {
	if a.Search != nil {
		a.Engine.Send(a.Search, msg)
	}
}

// recordMentions notes, off the actor, whom m calls out: named members,
// everyone for @channel, those online for @here, and the others of a
// conversation.
func (a *channel) recordMentions(m *models.Message) {
	found := mention.Parse(m.Content)
	direct := a.ch.Kind.Conversation()
	if !direct && len(found.Usernames) == 0 && !found.Channel && !found.Here {
		return
	}
	members, author, teamID := a.recipients, m.UserID.String(), a.ch.TeamID
	go func() {
		ctx, cancel := a.ctx()
		defer cancel()
		targets := map[string]bool{}
		switch {
		case direct || found.Channel:
			for id := range members {
				targets[id] = true
			}
		case found.Here:
			if online, err := actors.Ask[[]string](ctx, a.Engine, a.Hub, hub.Online{Members: members}); err == nil {
				for _, id := range online {
					targets[id] = true
				}
			}
		}
		if !direct && len(found.Usernames) > 0 {
			for _, name := range found.Usernames {
				if id, err := a.Repo.GetUserIDByUsername(ctx, name); err == nil {
					if _, ok := members[id.String()]; ok {
						targets[id.String()] = true
					}
				}
			}
		}
		delete(targets, author)
		userIDs := make([]gocql.UUID, 0, len(targets))
		for raw := range targets {
			if id, ok := ids.Parse(raw); ok {
				userIDs = append(userIDs, id)
			}
		}
		if err := a.Repo.AddMentions(ctx, a.id, m.ID, userIDs); err != nil {
			a.Logger.Warn("failed to record mentions", zap.String("channel", a.id.String()), zap.Error(err))
		}
		// Who a reply calls out follows its thread.
		if !ids.IsZero(m.RootID) {
			if err := a.Repo.TouchThread(ctx, userIDs, a.id, m.RootID, teamID, m.ID.Time(), gocql.UUID{}); err != nil {
				a.Logger.Warn("failed to follow a thread", zap.String("channel", a.id.String()), zap.Error(err))
			}
		}
	}()
}

// reply writes m to root's thread. Replies leave the channel's stream and
// its unread state alone: the root shows their count.
func (a *channel) reply(ctx context.Context, root, m *models.Message, author *models.User, atts []*models.Attachment) (*pb.Message, error) {
	m.RootID, m.Bucket = root.ID, 0
	if err := a.Repo.InsertReply(ctx, m); err != nil {
		if !scylla.Uncertain(err) {
			a.unclaim(atts, m.ID)
		}
		return nil, err
	}
	delete(a.typing, m.UserID)
	// The people to tell are those in the thread before this reply.
	notify := make([]string, 0, len(root.Participants)+1)
	seen := map[gocql.UUID]bool{m.UserID: true}
	for _, id := range append([]gocql.UUID{root.UserID}, root.Participants...) {
		if !seen[id] {
			seen[id] = true
			notify = append(notify, id.String())
		}
	}
	out := convert.Message(m, convert.PublicUser(author), atts)
	a.broadcast(&pb.Event{Payload: &pb.Event_MessagePosted{MessagePosted: a.posted(out, notify)}}, m, root)
	a.recordMentions(m)
	a.file(index.Add{Doc: a.doc(m)})

	root.ReplyCount++
	root.LastReplyAt = m.ID.Time()
	if !slices.Contains(root.Participants, m.UserID) {
		root.Participants = append(root.Participants, m.UserID)
	}
	a.touch(append([]gocql.UUID{root.UserID}, root.Participants...), root.ID, m.ID.Time(), m.UserID)
	if err := a.Repo.SetThreadSummary(ctx, root, root.ReplyCount, root.LastReplyAt, m.UserID); err != nil {
		a.Logger.Warn("failed to count a reply", zap.String("channel", a.id.String()), zap.Error(err))
	}
	a.rootChanged(ctx, root)
	return out, nil
}

// rootChanged tells members about a root whose thread changed.
func (a *channel) rootChanged(ctx context.Context, root *models.Message) {
	out, err := a.render(ctx, []*models.Message{root})
	if err != nil {
		a.Logger.Warn("failed to render a thread's root", zap.String("channel", a.id.String()), zap.Error(err))
		return
	}
	a.broadcast(&pb.Event{Payload: &pb.Event_MessageEdited{MessageEdited: &pb.MessageEdited{Message: out[0]}}}, root)
}

// unclaim returns attachments claimed for a message that was not written to
// the uploads awaiting a post.
func (a *channel) unclaim(atts []*models.Attachment, messageID gocql.UUID) {
	ctx, cancel := a.ctx()
	defer cancel()
	for _, att := range atts {
		if err := a.Repo.UnclaimAttachment(ctx, att, messageID); err != nil {
			a.Logger.Warn("failed to release an attachment", zap.String("attachment", att.ID.String()), zap.Error(err))
		}
	}
}

// ownAttachments returns the attachments of attIDs, in order, refusing any
// not uploaded by userID to this channel or already posted.
func (a *channel) ownAttachments(ctx context.Context, userID gocql.UUID, attIDs []gocql.UUID) ([]*models.Attachment, error) {
	if len(attIDs) == 0 {
		return nil, nil
	}
	found, err := a.Repo.GetAttachments(ctx, attIDs)
	if err != nil {
		return nil, err
	}
	byID := make(map[gocql.UUID]*models.Attachment, len(found))
	for _, att := range found {
		byID[att.ID] = att
	}
	out := make([]*models.Attachment, 0, len(attIDs))
	for _, id := range attIDs {
		att := byID[id]
		if att == nil || att.ChannelID != a.id || att.UploaderID != userID || !ids.IsZero(att.MessageID) {
			return nil, ierrors.InvalidArgument("attachment is not available")
		}
		out = append(out, att)
	}
	return out, nil
}

// checkVoice refuses voice messages where the channel stopped them since
// they were recorded, and more than one in a message.
func (a *channel) checkVoice(atts []*models.Attachment) error {
	voices := 0
	for _, att := range atts {
		if att.Voice {
			voices++
		}
	}
	switch {
	case voices > 0 && a.ch.VoiceDisabled:
		return ierrors.FailedPrecondition("voice messages are turned off in this channel")
	case voices > 1:
		return ierrors.InvalidArgument("a message carries one voice message at most")
	}
	return nil
}

// messageOf finds a message, or with rawRoot a reply in that thread.
func (a *channel) messageOf(ctx context.Context, raw, rawRoot string) (*models.Message, error) {
	id, ok := ids.ParseTime(raw)
	if !ok {
		return nil, ierrors.InvalidArgument("invalid message id")
	}
	var (
		m   *models.Message
		err error
	)
	if rawRoot == "" {
		m, err = a.Repo.GetMessage(ctx, a.id, id)
	} else if rootID, ok := ids.ParseTime(rawRoot); !ok {
		return nil, ierrors.InvalidArgument("invalid message id")
	} else {
		m, err = a.Repo.GetReply(ctx, a.id, rootID, id)
	}
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, ierrors.NotFound("message not found")
		}
		return nil, err
	}
	return m, nil
}

func (a *channel) edit(msg Edit) (*pb.Message, error) {
	role, err := a.member(msg.P)
	if err != nil {
		return nil, err
	}
	if !a.Authz.Can(authz.ObjMessage, authz.ActEditOwn, string(role), string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	if a.archived() {
		return nil, errArchived
	}
	// Each edit reaches every member, as a post does: they share its pace.
	if !a.poster(msg.P.UserID).Allow() {
		return nil, ierrors.ResourceExhausted("you are editing too fast")
	}
	content, err := validate.Text("message", msg.Content, a.Cfg.MaxMessageLength)
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	m, err := a.messageOf(ctx, msg.MessageID, msg.RootID)
	if err != nil {
		return nil, err
	}
	if m.UserID != msg.P.UserID {
		return nil, ierrors.PermissionDenied()
	}
	if content == "" && len(m.Attachments) == 0 {
		return nil, ierrors.InvalidArgument("message is empty")
	}
	markup, err := markupOf(msg.P, msg.Markup)
	if err != nil {
		return nil, err
	}
	author, err := a.Users.Get(ctx, m.UserID)
	if err != nil {
		return nil, err
	}
	atts, err := a.Repo.GetAttachments(ctx, m.Attachments)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if ids.IsZero(m.RootID) {
		err = a.Repo.UpdateMessageContent(ctx, a.id, m.ID, content, now)
	} else {
		err = a.Repo.UpdateReplyContent(ctx, a.id, m.RootID, m.ID, content, now)
	}
	if err != nil {
		return nil, err
	}
	// A bot's edit sets its buttons, none when it gives none.
	if msg.P.Bot && !slices.Equal(markup, m.Markup) {
		if err := a.Repo.SetMarkup(ctx, a.id, m.RootID, m.ID, markup); err != nil {
			return nil, err
		}
		m.Markup = markup
	}
	old := m.Content
	m.Content, m.EditedAt = content, now
	a.file(index.Update{Doc: a.doc(m), Old: old})
	out := convert.Message(m, convert.PublicUser(author), orderAttachments(m.Attachments, atts))
	mine := proto.CloneOf(out)
	a.broadcast(&pb.Event{Payload: &pb.Event_MessageEdited{MessageEdited: &pb.MessageEdited{Message: out}}}, m)
	return mine, a.markSaved(ctx, msg.P.UserID, []*pb.Message{mine})
}

func (a *channel) delete(msg Delete) (Done, error) {
	role, err := a.member(msg.P)
	if err != nil {
		return Done{}, err
	}
	if !a.poster(msg.P.UserID).Allow() {
		return Done{}, ierrors.ResourceExhausted("you are deleting too fast")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	m, err := a.messageOf(ctx, msg.MessageID, msg.RootID)
	if err != nil {
		return Done{}, err
	}
	if m.UserID == msg.P.UserID {
		if !a.Authz.Can(authz.ObjMessage, authz.ActDeleteOwn, string(role), string(msg.P.Role)) {
			return Done{}, ierrors.PermissionDenied()
		}
	} else if msg.P.Bot {
		return Done{}, ierrors.PermissionDenied()
	} else {
		tr, err := a.teamRole(ctx, msg.P.UserID)
		if err != nil {
			return Done{}, err
		}
		if !a.Authz.Can(authz.ObjMessage, authz.ActDeleteAny, string(role), string(tr), string(msg.P.Role)) {
			return Done{}, ierrors.PermissionDenied()
		}
	}
	if !ids.IsZero(m.RootID) {
		return Done{}, a.deleteReply(ctx, m)
	}
	if err := a.Repo.DeleteMessage(ctx, a.id, m.ID); err != nil {
		return Done{}, err
	}
	a.file(index.Remove{Doc: a.doc(m)})
	a.unpinGone(ctx, m)
	a.dropReactions(ctx, m.ID)
	if m.ID == a.ch.LastMessageID {
		a.rewindLast(ctx)
	}
	a.dropAttachments(m.Attachments)
	if m.ReplyCount > 0 {
		a.dropThread(ctx, m.ID)
		if err := a.Repo.UnfollowThreads(ctx, append([]gocql.UUID{m.UserID}, m.Participants...), a.id, m.ID); err != nil {
			a.Logger.Warn("failed to drop a thread's follows", zap.String("channel", a.id.String()), zap.Error(err))
		}
	}
	a.broadcast(&pb.Event{Payload: &pb.Event_MessageDeleted{MessageDeleted: &pb.MessageDeleted{
		ChannelId: a.id.String(), MessageId: m.ID.String(),
	}}}, m)
	return Done{}, nil
}

func (a *channel) deleteReply(ctx context.Context, m *models.Message) error {
	if err := a.Repo.DeleteReply(ctx, a.id, m.RootID, m.ID); err != nil {
		return err
	}
	a.file(index.Remove{Doc: a.doc(m)})
	a.unpinGone(ctx, m)
	a.dropReactions(ctx, m.ID)
	a.dropAttachments(m.Attachments)
	a.broadcast(&pb.Event{Payload: &pb.Event_MessageDeleted{MessageDeleted: &pb.MessageDeleted{
		ChannelId: a.id.String(), MessageId: m.ID.String(), RootId: m.RootID.String(),
	}}}, m)
	root, err := a.Repo.GetMessage(ctx, a.id, m.RootID)
	if err != nil {
		return nil
	}
	newest, _, err := a.Repo.ListReplies(ctx, a.id, root.ID, gocql.UUID{}, 1)
	if err != nil {
		a.Logger.Warn("failed to recount a thread", zap.String("channel", a.id.String()), zap.Error(err))
		return nil
	}
	root.ReplyCount, root.LastReplyAt = max(root.ReplyCount-1, 0), time.Time{}
	if len(newest) > 0 {
		root.LastReplyAt = newest[0].ID.Time()
	} else {
		root.ReplyCount = 0
	}
	if err := a.Repo.SetThreadSummary(ctx, root, root.ReplyCount, root.LastReplyAt, gocql.UUID{}); err != nil {
		a.Logger.Warn("failed to recount a thread", zap.String("channel", a.id.String()), zap.Error(err))
	}
	a.rootChanged(ctx, root)
	return nil
}

// dropThread deletes the replies of a deleted root, their reactions and
// their files.
func (a *channel) dropThread(ctx context.Context, rootID gocql.UUID) {
	replies, err := a.Repo.ThreadContents(ctx, a.id, rootID)
	if err != nil {
		a.Logger.Warn("failed to list a thread's replies", zap.String("channel", a.id.String()), zap.Error(err))
	}
	if err := a.Repo.DeleteThread(ctx, a.id, rootID); err != nil {
		a.Logger.Warn("failed to delete a thread", zap.String("channel", a.id.String()), zap.Error(err))
		return
	}
	var atts []gocql.UUID
	for _, m := range replies {
		a.file(index.Remove{Doc: a.doc(m)})
		a.unpinGone(ctx, m)
		a.dropReactions(ctx, m.ID)
		atts = append(atts, m.Attachments...)
	}
	a.dropAttachments(atts)
}

func (a *channel) dropReactions(ctx context.Context, messageID gocql.UUID) {
	if err := a.Repo.DeleteReactions(ctx, a.id, messageID); err != nil {
		a.Logger.Warn("failed to delete reactions", zap.String("channel", a.id.String()), zap.Error(err))
	}
}

// maxReactionKinds bounds the different emoji on one message.
const maxReactionKinds = 20

func (a *channel) react(msg React) ([]*pb.Reaction, error) {
	if _, err := a.member(msg.P); err != nil {
		return nil, err
	}
	if a.archived() {
		return nil, errArchived
	}
	if !emoji.Valid(msg.Emoji) {
		return nil, ierrors.InvalidArgument("unknown emoji")
	}
	if !a.poster(msg.P.UserID).Allow() {
		return nil, ierrors.ResourceExhausted("you are reacting too fast")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	m, err := a.messageOf(ctx, msg.MessageID, msg.RootID)
	if err != nil {
		return nil, err
	}
	if msg.Remove {
		err = a.Repo.RemoveReaction(ctx, a.id, m.ID, msg.Emoji, msg.P.UserID)
	} else {
		var rows []*scylla.Reaction
		if rows, err = a.Repo.ListReactions(ctx, a.id, []gocql.UUID{m.ID}); err != nil {
			return nil, err
		}
		kinds := map[string]bool{}
		for _, r := range rows {
			kinds[r.Emoji] = true
		}
		if !kinds[msg.Emoji] && len(kinds) >= maxReactionKinds {
			return nil, ierrors.FailedPrecondition("this message has as many kinds of reaction as it can take")
		}
		err = a.Repo.AddReaction(ctx, a.id, m.ID, msg.Emoji, msg.P.UserID)
	}
	if err != nil {
		return nil, err
	}
	rows, err := a.Repo.ListReactions(ctx, a.id, []gocql.UUID{m.ID})
	if err != nil {
		return nil, err
	}
	out := groupReactions(rows)[m.ID]
	a.broadcast(&pb.Event{Payload: &pb.Event_ReactionsChanged{ReactionsChanged: &pb.ReactionsChanged{
		ChannelId: a.id.String(), MessageId: m.ID.String(), RootId: convert.ID(m.RootID), Reactions: out,
	}}}, m)
	return out, nil
}

// groupReactions gathers reactions per message, in the picker's order of
// emoji, people in the order stored.
func groupReactions(rows []*scylla.Reaction) map[gocql.UUID][]*pb.Reaction {
	byMessage := map[gocql.UUID]map[string]*pb.Reaction{}
	for _, r := range rows {
		m := byMessage[r.MessageID]
		if m == nil {
			m = map[string]*pb.Reaction{}
			byMessage[r.MessageID] = m
		}
		re := m[r.Emoji]
		if re == nil {
			re = &pb.Reaction{Emoji: r.Emoji}
			m[r.Emoji] = re
		}
		re.UserIds = append(re.UserIds, r.UserID.String())
	}
	out := make(map[gocql.UUID][]*pb.Reaction, len(byMessage))
	for id, m := range byMessage {
		for _, e := range emoji.All {
			if re := m[e.Name]; re != nil {
				out[id] = append(out[id], re)
			}
		}
	}
	return out
}

// rewindLast makes the newest remaining message the channel's last, so the
// channel does not stay unread for a message nobody can see.
func (a *channel) rewindLast(ctx context.Context) {
	list, _, err := a.Repo.ListMessages(ctx, a.id, gocql.UUID{}, 1)
	if err != nil {
		a.Logger.Warn("failed to find the last message", zap.String("channel", a.id.String()), zap.Error(err))
		return
	}
	var last gocql.UUID
	if len(list) > 0 {
		last = list[0].ID
	}
	if err := a.Repo.ResetLastMessage(ctx, a.id, last, time.Now()); err != nil {
		a.Logger.Warn("failed to record the last message", zap.String("channel", a.id.String()), zap.Error(err))
		return
	}
	a.ch.LastMessageID = last
}

// dropAttachments deletes a deleted message's files in the background.
func (a *channel) dropAttachments(attIDs []gocql.UUID) {
	if len(attIDs) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		atts, err := a.Repo.GetAttachments(ctx, attIDs)
		if err != nil {
			a.Logger.Warn("failed to read attachments to delete", zap.Error(err))
			return
		}
		for _, att := range atts {
			if att.ChannelID != a.id {
				continue
			}
			// The row goes first: a row left without its object would fail
			// every download, an object left without its row is pended.
			if err := a.Repo.DeleteAttachment(ctx, att); err != nil {
				a.Logger.Warn("failed to delete attachment", zap.String("attachment", att.ID.String()), zap.Error(err))
				continue
			}
			if err := a.Storage.Delete(ctx, att.ObjectKey); err != nil {
				a.Logger.Warn("failed to delete attachment object", zap.String("attachment", att.ID.String()), zap.Error(err))
				if err := a.Repo.PendObject(ctx, att.ID, att.ObjectKey, time.Now().UTC()); err != nil {
					a.Logger.Error("an attachment object is left in storage", zap.String("key", att.ObjectKey), zap.Error(err))
				}
			}
		}
	}()
}

func orderAttachments(order []gocql.UUID, atts []*models.Attachment) []*models.Attachment {
	byID := make(map[gocql.UUID]*models.Attachment, len(atts))
	for _, att := range atts {
		byID[att.ID] = att
	}
	out := make([]*models.Attachment, 0, len(order))
	for _, id := range order {
		if att := byID[id]; att != nil {
			out = append(out, att)
		}
	}
	return out
}

func (a *channel) canRead(p *models.Principal) error {
	if err := a.mayRead(p); err != nil {
		return err
	}
	return a.readsHistory(p)
}

// mayRead checks p may read the channel's messages, though a bot in privacy
// mode only those it was sent.
func (a *channel) mayRead(p *models.Principal) error {
	role, err := a.member(p)
	if err != nil {
		return err
	}
	if !a.Authz.Can(authz.ObjMessage, authz.ActRead, string(role), string(p.Role)) {
		return ierrors.PermissionDenied()
	}
	return nil
}

// history reads off the actor: the membership check happens here, in order,
// and the reply comes from the reading goroutine.
func (a *channel) history(c *actor.Context, msg History) error {
	if err := a.canRead(msg.P); err != nil {
		return err
	}
	var before, after gocql.UUID
	for raw, into := range map[string]*gocql.UUID{msg.BeforeID: &before, msg.AfterID: &after} {
		if raw == "" {
			continue
		}
		id, ok := ids.ParseTime(raw)
		if !ok {
			return ierrors.InvalidArgument("invalid cursor")
		}
		*into = id
	}
	limit := msg.Limit
	if limit <= 0 || limit > a.Cfg.HistoryLimit {
		limit = a.Cfg.HistoryLimit
	}
	userID, cleared := msg.P.UserID, a.cleared[msg.P.UserID]
	a.async(c, func(ctx context.Context) (any, error) {
		var (
			list []*models.Message
			more bool
			err  error
		)
		if ids.IsZero(after) {
			list, more, err = a.Repo.ListMessages(ctx, a.id, before, limit)
		} else if list, more, err = a.Repo.ListMessagesAfter(ctx, a.id, after, limit); err == nil {
			slices.Reverse(list)
		}
		if err != nil {
			return nil, err
		}
		// What the caller cleared ends their history.
		if n := len(list); !ids.IsZero(cleared) {
			if list = slices.DeleteFunc(list, func(m *models.Message) bool { return !ids.After(m.ID, cleared) }); len(list) < n && ids.IsZero(after) {
				more = false
			}
		}
		out, err := a.render(ctx, list)
		if err != nil {
			return nil, err
		}
		return &HistoryResult{Messages: out, HasMore: more}, a.markSaved(ctx, userID, out)
	})
	return nil
}

func (a *channel) getMessage(c *actor.Context, msg GetMessage) error {
	if err := a.mayRead(msg.P); err != nil {
		return err
	}
	cleared, bot := a.cleared[msg.P.UserID], a.privacyBot(msg.P)
	a.async(c, func(ctx context.Context) (any, error) {
		m, err := a.messageOf(ctx, msg.MessageID, msg.RootID)
		if err != nil {
			return nil, err
		}
		if !shown(m, cleared) || (bot != nil && !a.sentTo(ctx, msg.P.UserID, bot, m)) {
			return nil, ierrors.NotFound("message not found")
		}
		out, err := a.render(ctx, []*models.Message{m})
		if err != nil {
			return nil, err
		}
		return out[0], a.markSaved(ctx, msg.P.UserID, out)
	})
	return nil
}

func (a *channel) thread(c *actor.Context, msg Thread) error {
	if err := a.canRead(msg.P); err != nil {
		return err
	}
	var before gocql.UUID
	if msg.BeforeID != "" {
		id, ok := ids.ParseTime(msg.BeforeID)
		if !ok {
			return ierrors.InvalidArgument("invalid cursor")
		}
		before = id
	}
	limit := msg.Limit
	if limit <= 0 || limit > a.Cfg.HistoryLimit {
		limit = a.Cfg.HistoryLimit
	}
	cleared := a.cleared[msg.P.UserID]
	a.async(c, func(ctx context.Context) (any, error) {
		root, err := a.messageOf(ctx, msg.RootID, "")
		if err != nil {
			return nil, err
		}
		if !shown(root, cleared) {
			return nil, ierrors.NotFound("message not found")
		}
		replies, more, err := a.Repo.ListReplies(ctx, a.id, root.ID, before, limit)
		if err != nil {
			return nil, err
		}
		slices.Reverse(replies)
		out, err := a.render(ctx, append([]*models.Message{root}, replies...))
		if err != nil {
			return nil, err
		}
		following, err := a.Repo.GetThreadFollow(ctx, msg.P.UserID, a.id, root.ID)
		if err != nil {
			return nil, err
		}
		return &ThreadResult{Root: out[0], Replies: out[1:], HasMore: more, Following: following}, a.markSaved(ctx, msg.P.UserID, out)
	})
	return nil
}

// render attaches authors and attachments to stored messages.
func (a *channel) render(ctx context.Context, list []*models.Message) ([]*pb.Message, error) {
	return Render(a.Deps)(ctx, a.id, list)
}

// Render renders a channel's stored messages with their authors,
// attachments and reactions.
func Render(d *Deps) updates.Renderer {
	return func(ctx context.Context, channelID gocql.UUID, list []*models.Message) ([]*pb.Message, error) {
		return render(ctx, d, channelID, list)
	}
}

func render(ctx context.Context, d *Deps, channelID gocql.UUID, list []*models.Message) ([]*pb.Message, error) {
	userIDs := make([]gocql.UUID, 0, len(list))
	var attIDs []gocql.UUID
	for _, m := range list {
		userIDs = append(userIDs, m.UserID)
		attIDs = append(attIDs, m.Attachments...)
	}
	authors, err := d.Users.GetMany(ctx, userIDs)
	if err != nil {
		return nil, err
	}
	atts := make(map[gocql.UUID]*models.Attachment, len(attIDs))
	for start := 0; start < len(attIDs); start += 100 {
		found, err := d.Repo.GetAttachments(ctx, attIDs[start:min(start+100, len(attIDs))])
		if err != nil {
			return nil, err
		}
		for _, att := range found {
			atts[att.ID] = att
		}
	}
	msgIDs := make([]gocql.UUID, len(list))
	for i, m := range list {
		msgIDs[i] = m.ID
	}
	rows, err := d.Repo.ListReactions(ctx, channelID, msgIDs)
	if err != nil {
		return nil, err
	}
	reactions := groupReactions(rows)
	out := make([]*pb.Message, 0, len(list))
	for _, m := range list {
		var mine []*models.Attachment
		for _, id := range m.Attachments {
			if att := atts[id]; att != nil {
				mine = append(mine, att)
			}
		}
		view := convert.Message(m, convert.PublicUser(authors[m.UserID]), mine)
		view.Reactions = reactions[m.ID]
		out = append(out, view)
	}
	return out, nil
}

func (a *channel) typingEvent(msg Typing) (Done, error) {
	if _, err := a.member(msg.P); err != nil || a.archived() {
		return Done{}, err
	}
	now := time.Now()
	if last, ok := a.typing[msg.P.UserID]; ok && now.Sub(last) < a.Cfg.TypingInterval {
		return Done{}, nil
	}
	a.typing[msg.P.UserID] = now
	ctx, cancel := a.ctx()
	defer cancel()
	u, err := a.Users.Get(ctx, msg.P.UserID)
	if err != nil {
		return Done{}, err
	}
	// Only the members showing the channel see who types.
	a.Engine.Send(a.Hub, hub.Deliver{
		Members: a.recipients, Viewing: a.id.String(), Except: msg.P.UserID.String(),
		Event: &pb.Event{Payload: &pb.Event_Typing{Typing: &pb.Typing{ChannelId: a.id.String(), User: convert.PublicUser(u)}}},
	})
	return Done{}, nil
}

// markRead writes off the actor: every reader of a busy channel marks it.
func (a *channel) markRead(c *actor.Context, msg MarkRead) error {
	if _, err := a.member(msg.P); err != nil {
		return err
	}
	id, ok := ids.ParseTime(msg.MessageID)
	if !ok || id.Time().After(time.Now().Add(time.Minute)) {
		return ierrors.InvalidArgument("invalid message id")
	}
	userID := msg.P.UserID
	a.async(c, func(ctx context.Context) (any, error) {
		if err := a.Repo.SetLastRead(ctx, userID, a.id, id); err != nil {
			return nil, err
		}
		return Done{}, a.Repo.ClearMentions(ctx, userID, a.id)
	})
	return nil
}

func (a *channel) addLocal(ctx context.Context, userID gocql.UUID, role models.ChannelRole) error {
	m, bot, err := a.newMember(ctx, userID, role)
	if err != nil {
		return err
	}
	if err := a.Repo.AddChannelMember(ctx, m); err != nil {
		return err
	}
	a.members[userID] = role
	if bot != nil {
		a.bots[userID] = bot
	}
	a.snapshot()
	return nil
}

func (a *channel) removeLocal(ctx context.Context, userID gocql.UUID) error {
	m := &models.ChannelMember{ChannelID: a.id, UserID: userID, TeamID: a.ch.TeamID}
	if err := a.Repo.RemoveChannelMember(ctx, m); err != nil {
		return err
	}
	delete(a.members, userID)
	delete(a.typing, userID)
	delete(a.posters, userID)
	delete(a.bots, userID)
	a.snapshot()
	return nil
}

func (a *channel) joined(userID gocql.UUID, role models.ChannelRole) (*pb.Channel, error) {
	view, err := a.channelFor(userID, role)
	if err != nil {
		return nil, err
	}
	a.deliver(userID, &pb.Event{Payload: &pb.Event_ChannelJoined{ChannelJoined: &pb.ChannelJoined{Channel: view}}})
	return view, nil
}

func (a *channel) left(userID gocql.UUID, own bool) {
	a.deliver(userID, &pb.Event{Payload: &pb.Event_ChannelLeft{ChannelLeft: &pb.ChannelLeft{
		ChannelId: a.id.String(), TeamId: convert.ID(a.ch.TeamID), Own: own,
	}}})
}

func (a *channel) join(msg Join) (*pb.Channel, error) {
	if role, ok := a.members[msg.P.UserID]; ok {
		return a.channelFor(msg.P.UserID, role)
	}
	if a.ch.Kind != models.ChannelPublic {
		return nil, ierrors.NotFound("channel not found")
	}
	if a.archived() && !msg.System {
		return nil, errArchived
	}
	ctx, cancel := a.ctx()
	defer cancel()
	tr, err := a.teamRole(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	if tr == "" {
		return nil, ierrors.NotFound("channel not found")
	}
	if !msg.System && !a.Authz.Can(authz.ObjChannel, authz.ActJoin, string(tr), string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	if err := a.addTeamMember(ctx, msg.P.UserID); err != nil {
		return nil, err
	}
	return a.joined(msg.P.UserID, models.ChannelRoleMember)
}

// addTeamMember adds a member of the channel's team, checking again once
// added: a removal from the team lists the channels after it, so a join it
// missed is undone here.
func (a *channel) addTeamMember(ctx context.Context, userID gocql.UUID) error {
	if err := a.addLocal(ctx, userID, models.ChannelRoleMember); err != nil {
		return err
	}
	in, err := a.inTeam(ctx, a.ch.TeamID, userID)
	if err == nil && in {
		return nil
	}
	if rerr := a.removeLocal(ctx, userID); rerr != nil {
		a.Logger.Error("failed to undo a join", zap.String("channel", a.id.String()), zap.Error(rerr))
	}
	if err != nil {
		return err
	}
	return ierrors.NotFound("channel not found")
}

func (a *channel) leave(msg Leave) (Done, error) {
	if _, err := a.member(msg.P); err != nil {
		return Done{}, err
	}
	if a.ch.Kind.Conversation() {
		return Done{}, ierrors.FailedPrecondition("conversations cannot be left")
	}
	if a.ch.Name == DefaultChannel {
		return Done{}, ierrors.FailedPrecondition("every team member stays in this channel")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	if err := a.removeLocal(ctx, msg.P.UserID); err != nil {
		return Done{}, err
	}
	a.left(msg.P.UserID, true)
	return Done{}, nil
}

func (a *channel) addMember(msg AddMember) (Done, error) {
	role, err := a.member(msg.P)
	if err != nil {
		return Done{}, err
	}
	if a.ch.Kind.Conversation() {
		return Done{}, ierrors.FailedPrecondition("a conversation keeps the people it started with")
	}
	if !a.Authz.Can(authz.ObjChannelMember, authz.ActAdd, string(role), string(msg.P.Role)) {
		return Done{}, ierrors.PermissionDenied()
	}
	if a.archived() {
		return Done{}, errArchived
	}
	target, ok := ids.Parse(msg.UserID)
	if !ok {
		return Done{}, ierrors.InvalidArgument("invalid user id")
	}
	if _, exists := a.members[target]; exists {
		return Done{}, nil
	}
	ctx, cancel := a.ctx()
	defer cancel()
	tr, err := a.teamRole(ctx, target)
	if err != nil {
		return Done{}, err
	}
	if tr == "" {
		return Done{}, ierrors.InvalidArgument("only team members can be added")
	}
	if err := a.mayAddBot(ctx, msg.P, role, target); err != nil {
		return Done{}, err
	}
	if err := a.addTeamMember(ctx, target); err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return Done{}, ierrors.InvalidArgument("only team members can be added")
		}
		return Done{}, err
	}
	if _, err := a.joined(target, models.ChannelRoleMember); err != nil {
		return Done{}, err
	}
	return Done{}, nil
}

func (a *channel) removeMember(msg RemoveMember) (Done, error) {
	if _, ok := a.members[msg.UserID]; !ok {
		if msg.System {
			return Done{}, nil
		}
		return Done{}, ierrors.NotFound("not a member")
	}
	if a.ch.Kind.Conversation() && !msg.System {
		return Done{}, ierrors.FailedPrecondition("a conversation keeps the people it started with")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	if !msg.System {
		role := a.members[msg.P.UserID]
		tr, err := a.teamRole(ctx, msg.P.UserID)
		if err != nil {
			return Done{}, err
		}
		if !a.Authz.Can(authz.ObjChannelMember, authz.ActRemove, string(role), string(tr), string(msg.P.Role)) {
			return Done{}, ierrors.PermissionDenied()
		}
		if a.ch.Name == DefaultChannel {
			return Done{}, ierrors.FailedPrecondition("every team member stays in this channel")
		}
	}
	if err := a.removeLocal(ctx, msg.UserID); err != nil {
		return Done{}, err
	}
	a.left(msg.UserID, false)
	return Done{}, nil
}

func (a *channel) listMembers(c *actor.Context, msg Members) error {
	if _, err := a.member(msg.P); err != nil {
		return err
	}
	roles := make(map[gocql.UUID]models.ChannelRole, len(a.members))
	userIDs := make([]gocql.UUID, 0, len(a.members))
	for id, role := range a.members {
		roles[id] = role
		userIDs = append(userIDs, id)
	}
	a.async(c, func(ctx context.Context) (any, error) {
		found, err := a.Users.GetMany(ctx, userIDs)
		if err != nil {
			return nil, err
		}
		out := make([]*pb.ChannelMember, 0, len(found))
		for id, u := range found {
			out = append(out, &pb.ChannelMember{User: convert.PublicUser(u), Role: convert.ChannelRole(roles[id])})
		}
		return out, nil
	})
	return nil
}

// async runs a read off the actor and replies from its goroutine. fn may only
// touch what does not change after the actor loaded.
func (a *channel) async(c *actor.Context, fn func(ctx context.Context) (any, error)) {
	sender, engine := c.Sender(), c.Engine()
	if !actors.IsAsk(sender) {
		return
	}
	go func() {
		ctx, cancel := a.ctx()
		defer cancel()
		out, err := fn(ctx)
		if err != nil {
			if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
				a.Logger.Error("channel read failed", append(e.Fields(), zap.String("channel", a.id.String()))...)
			}
			engine.Send(sender, err)
			return
		}
		engine.Send(sender, out)
	}()
}
