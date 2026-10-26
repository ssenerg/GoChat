package channels

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/mention"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/callbacks"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/updates"
)

// botMember is a bot of the channel. Bots get the channel's updates in
// their own account, numbered there, rather than with the members.
type botMember struct {
	// seesAll is set for a bot that gets every message; otherwise it gets
	// those that speak to it.
	seesAll  bool
	username string
}

// The limits of a message's buttons.
const (
	maxButtonRows   = 8
	maxRowButtons   = 8
	maxButtonText   = 64
	maxCallbackData = 64
	maxButtonURL    = 2048
)

// loadBots notes the channel's bots among its members.
func (a *channel) loadBots(ctx context.Context, members []*models.ChannelMember) error {
	a.bots = make(map[gocql.UUID]*botMember)
	var botIDs []gocql.UUID
	for _, m := range members {
		if m.Bot {
			a.bots[m.UserID] = &botMember{seesAll: m.BotSeesAll}
			botIDs = append(botIDs, m.UserID)
		}
	}
	if len(botIDs) == 0 {
		return nil
	}
	found, err := a.Users.GetMany(ctx, botIDs)
	if err != nil {
		return err
	}
	for id, u := range found {
		a.bots[id].username = u.Username
	}
	return nil
}

// newMember is userID's membership as it joins: a bot gets every message
// of a conversation, and of a channel when it is set to.
func (a *channel) newMember(ctx context.Context, userID gocql.UUID, role models.ChannelRole) (*models.ChannelMember, *botMember, error) {
	m := &models.ChannelMember{ChannelID: a.id, UserID: userID, TeamID: a.ch.TeamID, Role: role, JoinedAt: time.Now().UTC()}
	u, err := a.Users.Get(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	if !u.Bot {
		return m, nil, nil
	}
	m.Bot, m.BotSeesAll = true, a.ch.Kind.Conversation()
	if !m.BotSeesAll {
		bot, err := a.Repo.GetBot(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		m.BotSeesAll = bot.SeesAll
	}
	return m, &botMember{seesAll: m.BotSeesAll, username: u.Username}, nil
}

// toBots hands ev, about m in root's thread when they are set, to the
// channel's bots that see it.
func (a *channel) toBots(ev *pb.Event, m, root *models.Message) {
	if len(a.bots) == 0 || a.Updates == nil {
		return
	}
	// A reply speaks to the bots of its thread: its root is read once.
	if m != nil && root == nil && !ids.IsZero(m.RootID) && a.privacyBots() {
		ctx, cancel := a.ctx()
		root, _ = a.Repo.GetMessage(ctx, a.id, m.RootID)
		cancel()
	}
	var to []gocql.UUID
	for id, b := range a.bots {
		if a.botSees(id, b, ev, m, root) {
			to = append(to, id)
		}
	}
	if len(to) > 0 {
		a.Engine.Send(a.Updates, updates.Push{UserIDs: to, Event: ev})
	}
}

// botSees reports whether a bot gets ev: never the post of its own
// message; in privacy mode, only what is about a message that speaks to
// it, and of the channel's own changes only those to the channel itself.
func (a *channel) botSees(id gocql.UUID, b *botMember, ev *pb.Event, m, root *models.Message) bool {
	if m != nil && m.UserID == id && ev.GetMessagePosted() != nil {
		return false
	}
	if b.seesAll || a.ch.Kind.Conversation() {
		return true
	}
	if m == nil {
		return ev.GetChannelUpdated() != nil
	}
	return addressed(id, b, m, root)
}

// privacyBots reports whether the channel has a bot in privacy mode.
func (a *channel) privacyBots() bool {
	if a.ch.Kind.Conversation() {
		return false
	}
	for _, b := range a.bots {
		if !b.seesAll {
			return true
		}
	}
	return false
}

// addressed reports whether m, in root's thread when it is a reply,
// speaks to a bot in privacy mode: it is the bot's, a command, mentions
// the bot, or is in a thread the bot is part of.
func addressed(id gocql.UUID, b *botMember, m, root *models.Message) bool {
	if m.UserID == id || strings.HasPrefix(strings.TrimSpace(m.Content), "/") {
		return true
	}
	if slices.Contains(mention.Parse(m.Content).Usernames, b.username) {
		return true
	}
	if ids.IsZero(m.RootID) {
		return slices.Contains(m.Participants, id)
	}
	return root != nil && (root.UserID == id || slices.Contains(root.Participants, id))
}

// privacyBot is p's membership when p is a bot in privacy mode.
func (a *channel) privacyBot(p *models.Principal) *botMember {
	if b := a.bots[p.UserID]; p.Bot && b != nil && !b.seesAll {
		cp := *b
		return &cp
	}
	return nil
}

// sentTo reports whether a bot in privacy mode was sent m. It runs off the
// actor.
func (a *channel) sentTo(ctx context.Context, id gocql.UUID, b *botMember, m *models.Message) bool {
	var root *models.Message
	if !ids.IsZero(m.RootID) {
		root, _ = a.Repo.GetMessage(ctx, a.id, m.RootID)
	}
	return addressed(id, b, m, root)
}

// readsHistory refuses a bot in privacy mode what it was not sent.
func (a *channel) readsHistory(p *models.Principal) error {
	if !p.Bot {
		return nil
	}
	if b := a.bots[p.UserID]; b == nil || !b.seesAll {
		return ierrors.New(ierrors.CodePermissionDenied, "a bot in privacy mode reads only what it is sent")
	}
	return nil
}

// markupOf checks the buttons p puts under a message, in their stored
// form; only bots put any.
func markupOf(p *models.Principal, markup *pb.Markup) ([]byte, error) {
	if len(markup.GetRows()) == 0 {
		return nil, nil
	}
	if !p.Bot {
		return nil, ierrors.InvalidArgument("only bots put buttons under messages")
	}
	if len(markup.Rows) > maxButtonRows {
		return nil, ierrors.InvalidArgument("a message has at most 8 rows of buttons")
	}
	for _, row := range markup.Rows {
		if len(row.Buttons) == 0 || len(row.Buttons) > maxRowButtons {
			return nil, ierrors.InvalidArgument("a row has 1 to 8 buttons")
		}
		for _, btn := range row.Buttons {
			if n := utf8.RuneCountInString(strings.TrimSpace(btn.Text)); n == 0 || n > maxButtonText || !utf8.ValidString(btn.Text) {
				return nil, ierrors.InvalidArgument("a button's text is 1 to 64 characters")
			}
			switch act := btn.Action.(type) {
			case *pb.Button_CallbackData:
				if len(act.CallbackData) == 0 || len(act.CallbackData) > maxCallbackData {
					return nil, ierrors.InvalidArgument("a button's data is 1 to 64 bytes")
				}
			case *pb.Button_Url:
				u, err := url.Parse(act.Url)
				if err != nil || len(act.Url) > maxButtonURL || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
					return nil, ierrors.InvalidArgument("a button's link is an http or https URL")
				}
			default:
				return nil, ierrors.InvalidArgument("a button has data or a link")
			}
		}
	}
	return proto.Marshal(markup)
}

// press presses a button of a bot's message: the bot gets a CallbackQuery,
// whose id the presser gets back with the bot's answer.
func (a *channel) press(msg Press) (string, error) {
	if msg.P.Bot {
		return "", ierrors.PermissionDenied()
	}
	if err := a.canRead(msg.P); err != nil {
		return "", err
	}
	if a.archived() {
		return "", errArchived
	}
	if !a.poster(msg.P.UserID).Allow() {
		return "", ierrors.ResourceExhausted("you are pressing too fast")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	m, err := a.messageOf(ctx, msg.MessageID, msg.RootID)
	if err != nil {
		return "", err
	}
	if !shown(m, a.cleared[msg.P.UserID]) {
		return "", ierrors.NotFound("message not found")
	}
	if _, ok := a.bots[m.UserID]; !ok {
		return "", ierrors.FailedPrecondition("the bot is no longer in this channel")
	}
	var markup pb.Markup
	if err := proto.Unmarshal(m.Markup, &markup); err != nil || !hasButton(&markup, msg.Data) {
		return "", ierrors.InvalidArgument("the message has no such button")
	}
	out, err := a.render(ctx, []*models.Message{m})
	if err != nil {
		return "", err
	}
	presser, err := a.Users.Get(ctx, msg.P.UserID)
	if err != nil {
		return "", err
	}
	id := ids.New().String()
	a.Engine.Send(a.Callbacks, callbacks.Pending{QueryID: id, Presser: msg.P.UserID, Bot: m.UserID, Expires: time.Now().Add(callbacks.TTL)})
	a.Engine.Send(a.Updates, updates.Push{UserIDs: []gocql.UUID{m.UserID}, Event: &pb.Event{Payload: &pb.Event_CallbackQuery{CallbackQuery: &pb.CallbackQuery{
		Id: id, From: convert.PublicUser(presser), Message: out[0], Data: msg.Data,
	}}}})
	return id, nil
}

func hasButton(markup *pb.Markup, data string) bool {
	for _, row := range markup.Rows {
		for _, btn := range row.Buttons {
			if d, ok := btn.Action.(*pb.Button_CallbackData); ok && d.CallbackData == data {
				return true
			}
		}
	}
	return false
}

// botIDs lists the channel's bots to one of its members.
func (a *channel) botIDs(msg BotMembers) ([]gocql.UUID, error) {
	if _, err := a.member(msg.P); err != nil {
		return nil, err
	}
	out := make([]gocql.UUID, 0, len(a.bots))
	for id := range a.bots {
		out = append(out, id)
	}
	return out, nil
}

// mayAddBot refuses adding a bot to a private channel to all but its
// admins: whatever the bot reads, its owner may read.
func (a *channel) mayAddBot(ctx context.Context, p *models.Principal, role models.ChannelRole, target gocql.UUID) error {
	if a.ch.Kind != models.ChannelPrivate || role == models.ChannelRoleAdmin || p.Role == models.SystemRoleAdmin {
		return nil
	}
	u, err := a.Users.Get(ctx, target)
	if err != nil || !u.Bot {
		return err
	}
	tr, err := a.teamRole(ctx, p.UserID)
	if err != nil {
		return err
	}
	if tr != models.TeamRoleAdmin {
		a.Logger.Debug("refused a bot in a private channel", zap.String("channel", a.id.String()))
		return ierrors.New(ierrors.CodePermissionDenied, "only the channel's admins add bots to it")
	}
	return nil
}
