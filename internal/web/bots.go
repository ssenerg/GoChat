package web

import (
	"cmp"
	"net/http"
	"regexp"
	"slices"
	"strings"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
)

// botsData is the settings' bots: the caller's, the teams they may add
// them to, and a token shown once.
type botsData struct {
	List          []*pb.Bot
	Teams         []*pb.Team
	Token         *shownToken
	Status, Error string
}

type shownToken struct {
	Bot, Value string
}

var commandPrefixRe = regexp.MustCompile(`^[a-z0-9_]{0,32}$`)

// loadBots reads the settings' bots section.
func (h *Handler) loadBots(r *http.Request) (botsData, error) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	bots, err := h.client.Bots.ListBots(ctx, &pb.ListBotsRequest{})
	if err != nil {
		return botsData{}, err
	}
	teams, err := h.client.Teams.ListTeams(ctx, &pb.ListTeamsRequest{})
	if err != nil {
		return botsData{}, err
	}
	slices.SortFunc(bots.Bots, func(a, b *pb.Bot) int { return cmp.Compare(a.GetUser().GetUsername(), b.GetUser().GetUsername()) })
	return botsData{List: bots.Bots, Teams: teams.Teams}, nil
}

// botsFragment answers with the bots section after a change to it.
func (h *Handler) botsFragment(w http.ResponseWriter, r *http.Request, err error, ok string, token *shownToken) {
	d, lerr := h.loadBots(r)
	if lerr != nil {
		h.fail(w, r, lerr)
		return
	}
	if err != nil {
		d.Error = drpc.Err(err).Message
	} else {
		d.Status, d.Token = ok, token
	}
	h.fragment(w, r, http.StatusOK, "bots", d)
}

func (h *Handler) createBot(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Bots.CreateBot(ctx, &pb.CreateBotRequest{
		Username: r.PostFormValue("username"), DisplayName: r.PostFormValue("display_name"), Description: r.PostFormValue("description"),
	})
	if err != nil {
		h.botsFragment(w, r, err, "", nil)
		return
	}
	h.botsFragment(w, r, nil, "", &shownToken{Bot: res.Bot.GetUser().GetUsername(), Value: res.Token})
}

func (h *Handler) updateBot(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	id := r.PathValue("bot")
	if !uuidRe.MatchString(id) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	_, err := h.client.Bots.UpdateBot(ctx, &pb.UpdateBotRequest{
		BotId: id, DisplayName: r.PostFormValue("display_name"), Description: r.PostFormValue("description"),
		SeesAll: r.PostFormValue("sees_all") == "1",
	})
	h.botsFragment(w, r, err, "Saved.", nil)
}

func (h *Handler) resetBotToken(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	id := r.PathValue("bot")
	if !uuidRe.MatchString(id) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Bots.ResetBotToken(ctx, &pb.ResetBotTokenRequest{BotId: id})
	if err != nil {
		h.botsFragment(w, r, err, "", nil)
		return
	}
	name := ""
	if d, err := h.loadBots(r); err == nil {
		for _, b := range d.List {
			if b.GetUser().GetId() == id {
				name = b.GetUser().GetUsername()
			}
		}
	}
	h.botsFragment(w, r, nil, "", &shownToken{Bot: name, Value: res.Token})
}

func (h *Handler) deleteBot(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	id := r.PathValue("bot")
	if !uuidRe.MatchString(id) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	_, err := h.client.Bots.DeleteBot(ctx, &pb.DeleteBotRequest{BotId: id})
	h.botsFragment(w, r, err, "The bot was deleted.", nil)
}

func (h *Handler) addBotToTeam(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	id, team := r.PathValue("bot"), r.PostFormValue("team_id")
	if !uuidRe.MatchString(id) || !uuidRe.MatchString(team) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	_, err := h.client.Bots.AddBotToTeam(ctx, &pb.AddBotToTeamRequest{BotId: id, TeamId: team})
	h.botsFragment(w, r, err, "The bot joined the team: add it to channels from their members list.", nil)
}

// commandSuggestions suggests the commands of the channel's bots that
// start with what follows the / being typed.
func (h *Handler) commandSuggestions(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	chID, q := r.PathValue("channel"), strings.ToLower(r.URL.Query().Get("q"))
	if !uuidRe.MatchString(chID) || !commandPrefixRe.MatchString(q) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Bots.ListChannelCommands(ctx, &pb.ListChannelCommandsRequest{ChannelId: chID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	slices.SortFunc(res.Commands, func(a, b *pb.ChannelCommand) int {
		return cmp.Compare(a.GetCommand().GetName(), b.GetCommand().GetName())
	})
	var items []*pb.ChannelCommand
	for _, c := range res.Commands {
		if strings.HasPrefix(c.GetCommand().GetName(), q) && len(items) < maxSuggestions {
			items = append(items, c)
		}
	}
	h.fragment(w, r, http.StatusOK, "suggest-commands", items)
}
