// Package bots manages bots: accounts people own, which sign in with a
// token, offer commands, answer the buttons of their messages and may have
// their updates delivered to a webhook.
package bots

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	"github.com/ssenerg/gochat/internal/core/validate"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/callbacks"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/directory"
	"github.com/ssenerg/gochat/internal/services/users"
)

type Repo interface {
	CreateBot(ctx context.Context, u *models.User, b *models.Bot) error
	GetBot(ctx context.Context, id gocql.UUID) (*models.Bot, error)
	GetBots(ctx context.Context, ids []gocql.UUID) ([]*models.Bot, error)
	ListBotIDs(ctx context.Context, owner gocql.UUID) ([]gocql.UUID, error)
	UpdateBot(ctx context.Context, id gocql.UUID, description string, seesAll bool) error
	ReplaceBotToken(ctx context.Context, id gocql.UUID, old, hash []byte) error
	DeleteBot(ctx context.Context, b *models.Bot) error
	SetBotCommands(ctx context.Context, id gocql.UUID, commands []byte) error
	SetWebhook(ctx context.Context, id gocql.UUID, url, secret string) error
	SetDelivered(ctx context.Context, id gocql.UUID, pts int64) error
	SetWebhookError(ctx context.Context, id gocql.UUID, reason string, at time.Time) error
	ListWebhooks(ctx context.Context) ([]*models.Bot, error)
	AccountPts(ctx context.Context, userID gocql.UUID) (int64, error)
}

type Deps struct {
	Logger    *zap.Logger
	Cfg       *Config
	Repo      Repo
	Users     *users.Cache
	Engine    *actor.Engine
	Directory *actor.PID
	Channels  *actor.PID
	Callbacks *actor.PID
	// Webhooks delivers the updates of bots with a webhook.
	Webhooks *actor.PID
	// Revoke stops tokens and closes what they opened.
	Revoke func(userID gocql.UUID, hashes [][]byte, all bool)
}

const (
	maxDescription = 512
	maxCommands    = 100
)

var commandRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

func Pool(d *Deps) actor.Producer {
	return actors.NewPool(d.Cfg.Workers, func() actor.Receiver { return &worker{Deps: d} },
		actor.WithMiddleware(actors.Recover(d.Logger)))
}

type worker struct{ *Deps }

func (w *worker) Receive(c *actor.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	defer cancel()
	var (
		out any
		err error
	)
	switch msg := c.Message().(type) {
	case Create:
		out, err = w.create(ctx, msg)
	case List:
		out, err = w.list(ctx, msg.P)
	case Update:
		out, err = w.update(ctx, msg)
	case ResetToken:
		out, err = w.resetToken(ctx, msg)
	case Delete:
		out, err = w.delete(ctx, msg)
	case AddToTeam:
		out, err = w.addToTeam(ctx, msg)
	case ChannelCommands:
		out, err = w.channelCommands(ctx, msg)
	case SetCommands:
		out, err = w.setCommands(ctx, msg)
	case GetCommands:
		out, err = w.getCommands(ctx, msg.P)
	case Answer:
		out, err = w.answer(ctx, msg)
	case SetWebhook:
		out, err = w.setWebhook(ctx, msg)
	case WebhookInfo:
		out, err = w.webhookInfo(ctx, msg.P)
	case Confirm:
		out, err = w.confirm(ctx, msg)
	default:
		return
	}
	if err != nil {
		if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
			w.Logger.Error("bot request failed", e.Fields()...)
		}
		actors.Reply(c, err)
		return
	}
	actors.Reply(c, out)
}

// owned finds a bot of p's.
func (w *worker) owned(ctx context.Context, p *models.Principal, raw string) (*models.Bot, *models.User, error) {
	id, ok := ids.Parse(raw)
	if !ok || p.Bot {
		return nil, nil, ierrors.NotFound("bot not found")
	}
	b, err := w.Repo.GetBot(ctx, id)
	if ierrors.Is(err, ierrors.CodeNotFound) || (err == nil && b.OwnerID != p.UserID) {
		return nil, nil, ierrors.NotFound("bot not found")
	}
	if err != nil {
		return nil, nil, err
	}
	u, err := w.Users.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if u.Deactivated {
		return nil, nil, ierrors.NotFound("bot not found")
	}
	return b, u, nil
}

// view renders a bot for its owner.
func view(b *models.Bot, u *models.User) *pb.Bot {
	out := &pb.Bot{
		User: convert.PublicUser(u), Description: b.Description, SeesAll: b.SeesAll,
		CreatedAt: convert.Time(b.CreatedAt), WebhookUrl: b.WebhookURL,
	}
	out.Commands = commandsOf(b)
	return out
}

func commandsOf(b *models.Bot) []*pb.BotCommand {
	var cmds pb.GetCommandsResponse
	if len(b.Commands) == 0 || proto.Unmarshal(b.Commands, &cmds) != nil {
		return nil
	}
	return cmds.Commands
}

func (w *worker) create(ctx context.Context, msg Create) (*Created, error) {
	if msg.P.Bot {
		return nil, ierrors.PermissionDenied()
	}
	name, err := validate.Username(msg.Username)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(name, "bot") {
		return nil, ierrors.InvalidArgument("a bot's username ends in \"bot\", such as weather_bot")
	}
	display, err := validate.Line("display name", msg.DisplayName, 1, 64)
	if err != nil {
		return nil, err
	}
	description, err := validate.Text("description", msg.Description, maxDescription)
	if err != nil {
		return nil, err
	}
	owned, err := w.Repo.ListBotIDs(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	if len(owned) >= w.Cfg.MaxPerOwner {
		return nil, ierrors.ResourceExhausted("you have as many bots as you may")
	}
	token := secure.BotToken()
	now := time.Now().UTC()
	u := &models.User{
		ID: ids.New(), Username: name, DisplayName: display, Role: models.SystemRoleUser,
		CreatedAt: now, UpdatedAt: now, Bot: true, BotOwner: msg.P.UserID,
	}
	b := &models.Bot{ID: u.ID, OwnerID: msg.P.UserID, Description: description, TokenHash: secure.Hash(token), CreatedAt: now}
	if err := w.Repo.CreateBot(ctx, u, b); err != nil {
		return nil, err
	}
	w.Logger.Info("bot created", zap.String("bot", u.ID.String()), zap.String("owner", msg.P.UserID.String()))
	return &Created{Bot: view(b, u), Token: token}, nil
}

func (w *worker) list(ctx context.Context, p *models.Principal) ([]*pb.Bot, error) {
	if p.Bot {
		return nil, ierrors.PermissionDenied()
	}
	botIDs, err := w.Repo.ListBotIDs(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	found, err := w.Repo.GetBots(ctx, botIDs)
	if err != nil {
		return nil, err
	}
	people, err := w.Users.GetMany(ctx, botIDs)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Bot, 0, len(found))
	for _, b := range found {
		if u := people[b.ID]; u != nil && !u.Deactivated {
			out = append(out, view(b, u))
		}
	}
	return out, nil
}

func (w *worker) update(ctx context.Context, msg Update) (*pb.Bot, error) {
	b, u, err := w.owned(ctx, msg.P, msg.BotID)
	if err != nil {
		return nil, err
	}
	description, err := validate.Text("description", msg.Description, maxDescription)
	if err != nil {
		return nil, err
	}
	if msg.DisplayName != "" && msg.DisplayName != u.DisplayName {
		bot := &models.Principal{UserID: b.ID, Role: models.SystemRoleUser, Bot: true}
		if _, err := actors.Ask[*pb.User](ctx, w.Engine, w.Directory, directory.UpdateProfile{P: bot, DisplayName: msg.DisplayName}); err != nil {
			return nil, err
		}
		if u, err = w.Users.Get(ctx, b.ID); err != nil {
			return nil, err
		}
	}
	if err := w.Repo.UpdateBot(ctx, b.ID, description, msg.SeesAll); err != nil {
		return nil, err
	}
	b.Description, b.SeesAll = description, msg.SeesAll
	return view(b, u), nil
}

func (w *worker) resetToken(ctx context.Context, msg ResetToken) (string, error) {
	b, _, err := w.owned(ctx, msg.P, msg.BotID)
	if err != nil {
		return "", err
	}
	token := secure.BotToken()
	if err := w.Repo.ReplaceBotToken(ctx, b.ID, b.TokenHash, secure.Hash(token)); err != nil {
		return "", err
	}
	w.Revoke(b.ID, [][]byte{b.TokenHash}, false)
	return token, nil
}

func (w *worker) delete(ctx context.Context, msg Delete) (Done, error) {
	b, _, err := w.owned(ctx, msg.P, msg.BotID)
	if err != nil {
		return Done{}, err
	}
	if _, err := actors.Ask[directory.Done](ctx, w.Engine, w.Directory, directory.RemoveBot{P: msg.P, BotID: b.ID}); err != nil {
		return Done{}, err
	}
	if err := w.Repo.DeleteBot(ctx, b); err != nil {
		return Done{}, err
	}
	w.Users.Invalidate(b.ID)
	w.Revoke(b.ID, [][]byte{b.TokenHash}, true)
	if w.Webhooks != nil {
		w.Engine.Send(w.Webhooks, Hooked{BotID: b.ID})
	}
	w.Logger.Info("bot deleted", zap.String("bot", b.ID.String()))
	return Done{}, nil
}

func (w *worker) addToTeam(ctx context.Context, msg AddToTeam) (Done, error) {
	b, _, err := w.owned(ctx, msg.P, msg.BotID)
	if err != nil {
		return Done{}, err
	}
	if _, err := actors.Ask[directory.Done](ctx, w.Engine, w.Directory, directory.AddBot{P: msg.P, BotID: b.ID, TeamID: msg.TeamID}); err != nil {
		return Done{}, err
	}
	return Done{}, nil
}

// channelCommands lists the commands of a channel's bots to a member.
func (w *worker) channelCommands(ctx context.Context, msg ChannelCommands) ([]*pb.ChannelCommand, error) {
	id, ok := ids.Parse(msg.ChannelID)
	if !ok {
		return nil, ierrors.InvalidArgument("invalid channel id")
	}
	botIDs, err := actors.Ask[[]gocql.UUID](ctx, w.Engine, w.Channels, channels.BotMembers{Target: channels.Target{P: msg.P, ChannelID: id}})
	if err != nil || len(botIDs) == 0 {
		return nil, err
	}
	found, err := w.Repo.GetBots(ctx, botIDs)
	if err != nil {
		return nil, err
	}
	people, err := w.Users.GetMany(ctx, botIDs)
	if err != nil {
		return nil, err
	}
	var out []*pb.ChannelCommand
	for _, b := range found {
		u := people[b.ID]
		if u == nil || u.Deactivated {
			continue
		}
		for _, cmd := range commandsOf(b) {
			out = append(out, &pb.ChannelCommand{Command: cmd, Bot: convert.PublicUser(u)})
		}
	}
	return out, nil
}

func (w *worker) setCommands(ctx context.Context, msg SetCommands) (Done, error) {
	if !msg.P.Bot {
		return Done{}, ierrors.PermissionDenied()
	}
	if len(msg.Commands) > maxCommands {
		return Done{}, ierrors.InvalidArgument("a bot offers at most 100 commands")
	}
	seen := map[string]bool{}
	cmds := make([]*pb.BotCommand, 0, len(msg.Commands))
	for _, c := range msg.Commands {
		name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(c.GetName())), "/")
		if !commandRe.MatchString(name) {
			return Done{}, ierrors.InvalidArgument("a command's name is 1-32 lowercase letters, digits and underscores")
		}
		if seen[name] {
			return Done{}, ierrors.InvalidArgument("each command is named once")
		}
		seen[name] = true
		desc, err := validate.Line("a command's description", c.GetDescription(), 1, 256)
		if err != nil {
			return Done{}, err
		}
		cmds = append(cmds, &pb.BotCommand{Name: name, Description: desc})
	}
	b, err := proto.Marshal(&pb.GetCommandsResponse{Commands: cmds})
	if err != nil {
		return Done{}, ierrors.Internal(err)
	}
	return Done{}, w.Repo.SetBotCommands(ctx, msg.P.UserID, b)
}

func (w *worker) getCommands(ctx context.Context, p *models.Principal) ([]*pb.BotCommand, error) {
	if !p.Bot {
		return nil, ierrors.PermissionDenied()
	}
	b, err := w.Repo.GetBot(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return commandsOf(b), nil
}

func (w *worker) answer(ctx context.Context, msg Answer) (Done, error) {
	if !msg.P.Bot {
		return Done{}, ierrors.PermissionDenied()
	}
	text, err := validate.Line("the answer", msg.Text, 0, 200)
	if err != nil {
		return Done{}, err
	}
	u, err := w.Users.Get(ctx, msg.P.UserID)
	if err != nil {
		return Done{}, err
	}
	_, err = actors.Ask[callbacks.Done](ctx, w.Engine, w.Callbacks, callbacks.Answer{
		QueryID: msg.QueryID, Bot: convert.PublicUser(u), Text: text, Alert: msg.Alert,
	})
	return Done{}, err
}
