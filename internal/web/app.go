package web

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/emoji"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/web/gateway"
	"github.com/ssenerg/gochat/internal/web/render"
)

const (
	pageSize       = 50
	defaultChannel = "town-square"
)

var (
	uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,49}$`)
)

type appPage struct {
	Page
	Teams   []*pb.Team
	Team    *pb.Team
	OOB     bool
	Lists   *render.Lists
	Channel *render.ChannelView
	// Threads, when set, shows the threads view in place of a channel.
	Threads *threadsView
}

// signedIn returns the user, or answers with a sign-in redirect.
func (h *Handler) signedIn(w http.ResponseWriter, r *http.Request) *pb.User {
	u, err := h.currentUser(r)
	switch {
	case err != nil:
		h.fail(w, r, err)
	case u == nil:
		h.fail(w, r, ierrors.Unauthenticated())
	}
	return u
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Teams.ListTeams(ctx, &pb.ListTeamsRequest{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(res.Teams) == 0 {
		http.Redirect(w, r, "/teams/new", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/t/"+res.Teams[0].Name, http.StatusSeeOther)
}

type teamForm struct {
	Name, DisplayName, Description string
}

type noTeamData struct {
	Page
	CanCreate bool
	HasTeams  bool
	Form      teamForm
}

func (h *Handler) newTeamPage(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Teams.ListTeams(ctx, &pb.ListTeamsRequest{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "noteam", noTeamData{
		Page: Page{Title: "Create a team", CSRF: h.csrfToken(w, r), User: u}, CanCreate: true, HasTeams: len(res.Teams) > 0,
	})
}

func (h *Handler) createTeam(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	form := teamForm{Name: r.PostFormValue("name"), DisplayName: r.PostFormValue("display_name"), Description: r.PostFormValue("description")}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Teams.CreateTeam(ctx, &pb.CreateTeamRequest{Name: form.Name, DisplayName: form.DisplayName, Description: form.Description})
	if err != nil {
		msg, ok := formError(err)
		if !ok {
			h.fail(w, r, err)
			return
		}
		h.page(w, r, statusOf(drpc.Err(err).Code), "noteam", noTeamData{
			Page: Page{Title: "Create a team", CSRF: h.csrfToken(w, r), User: u, Error: msg}, CanCreate: true, Form: form,
		})
		return
	}
	http.Redirect(w, r, "/t/"+res.Team.Name, http.StatusSeeOther)
}

func (h *Handler) team(ctx context.Context, name string) (*pb.Team, error) {
	if !slugRe.MatchString(name) {
		return nil, ierrors.NotFound("team not found")
	}
	res, err := h.client.Teams.GetTeam(ctx, &pb.GetTeamRequest{Key: &pb.GetTeamRequest_Name{Name: name}})
	if err != nil {
		return nil, err
	}
	return res.Team, nil
}

func (h *Handler) defaultChannel(ctx context.Context, team *pb.Team) (string, error) {
	res, err := h.client.Channels.ListChannels(ctx, &pb.ListChannelsRequest{TeamId: team.Id})
	if err != nil {
		return "", err
	}
	var first string
	for _, ch := range res.Channels {
		if render.Conversation(ch) {
			continue
		}
		if ch.Name == defaultChannel {
			return ch.Id, nil
		}
		if first == "" {
			first = ch.Id
		}
	}
	if first == "" {
		return "", ierrors.NotFound("you are in no channel of this team")
	}
	return first, nil
}

func (h *Handler) teamHome(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	team, err := h.team(ctx, r.PathValue("team"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	chID, err := h.defaultChannel(ctx, team)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.navigate(w, r, "/t/"+team.Name+"/ch/"+chID, false)
}

func moderator(u *pb.User, team *pb.Team, ch *pb.Channel) bool {
	return u.GetRole() == pb.SystemRole_SYSTEM_ROLE_ADMIN ||
		team.GetRole() == pb.TeamRole_TEAM_ROLE_ADMIN ||
		ch.GetRole() == pb.ChannelRole_CHANNEL_ROLE_ADMIN
}

// channelView loads one channel and its newest messages as u sees them, or
// with focus the messages around that one.
func (h *Handler) channelView(ctx context.Context, u *pb.User, team *pb.Team, chID, focus string) (*render.ChannelView, error) {
	g, gctx := errgroup.WithContext(ctx)
	var (
		ch       *pb.Channel
		msgs     *pb.ListMessagesResponse
		anchored bool
	)
	g.Go(func() error {
		res, err := h.client.Channels.GetChannel(gctx, &pb.GetChannelRequest{ChannelId: chID})
		if err != nil {
			return err
		}
		ch = res.Channel
		return nil
	})
	g.Go(func() error {
		res, more, err := h.history(gctx, chID, focus)
		if err != nil {
			// A voice channel has no messages to list.
			if code := drpc.Err(err).Code; code == ierrors.CodePermissionDenied || code == ierrors.CodeFailedPrecondition {
				return nil
			}
			return err
		}
		msgs, anchored = res, more
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if ch.TeamId != "" && ch.TeamId != team.Id {
		return nil, ierrors.NotFound("channel not found")
	}
	view := &render.ChannelView{
		Team:      team,
		Channel:   ch,
		Title:     render.Title(ch),
		Member:    ch.Role != pb.ChannelRole_CHANNEL_ROLE_UNSPECIFIED,
		Moderator: moderator(u, team, ch),
		Archived:  ch.ArchivedAt != nil,
		MeID:      u.GetId(),
	}
	if ch.Type == pb.ChannelType_CHANNEL_TYPE_VOICE {
		view.Member = true
		if vs, err := h.client.Voice.ListVoiceStates(ctx, &pb.ListVoiceStatesRequest{TeamId: team.Id}); err == nil {
			view.Voice = render.VoiceStates(vs.Channels)[ch.Id]
		}
		return view, nil
	}
	if msgs != nil {
		view.Messages = h.render.Messages(msgs.Messages, u, view.Moderator)
		view.HasMore, view.Anchored = msgs.HasMore, anchored
		if n := len(msgs.Messages); n > 0 {
			view.Newest, view.Oldest = msgs.Messages[0].Id, msgs.Messages[n-1].Id
		}
		for _, m := range view.Messages {
			m.Focus = focus != "" && m.Id == focus
		}
	}
	if ch.Peer != nil {
		if p, err := h.client.Users.GetPresence(ctx, &pb.GetPresenceRequest{UserIds: []string{ch.Peer.Id}}); err == nil {
			view.PeerStatus = p.Statuses[ch.Peer.Id]
		}
	}
	return view, nil
}

// history loads a channel's newest messages, or with focus the messages
// around that one; more reports newer messages beyond those loaded.
func (h *Handler) history(ctx context.Context, chID, focus string) (*pb.ListMessagesResponse, bool, error) {
	if focus == "" {
		res, err := h.client.Messages.ListMessages(ctx, &pb.ListMessagesRequest{ChannelId: chID, Limit: pageSize})
		return res, false, err
	}
	g, gctx := errgroup.WithContext(ctx)
	var (
		target       *pb.Message
		older, newer *pb.ListMessagesResponse
	)
	g.Go(func() error {
		res, err := h.client.Messages.GetMessage(gctx, &pb.GetMessageRequest{ChannelId: chID, MessageId: focus})
		if err != nil {
			if code := drpc.Err(err).Code; code == ierrors.CodeNotFound || code == ierrors.CodeInvalidArgument {
				return nil
			}
			return err
		}
		target = res.Message
		return nil
	})
	g.Go(func() (err error) {
		older, err = h.client.Messages.ListMessages(gctx, &pb.ListMessagesRequest{ChannelId: chID, BeforeId: focus, Limit: pageSize / 2})
		return
	})
	g.Go(func() (err error) {
		newer, err = h.client.Messages.ListMessages(gctx, &pb.ListMessagesRequest{ChannelId: chID, AfterId: focus, Limit: pageSize / 2})
		return
	})
	if err := g.Wait(); err != nil {
		return nil, false, err
	}
	if target == nil || target.RootId != "" {
		return h.history(ctx, chID, "")
	}
	list := append(append(newer.Messages, target), older.Messages...)
	return &pb.ListMessagesResponse{Messages: list, HasMore: older.HasMore}, newer.HasMore, nil
}

func (h *Handler) channelPage(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	chID := r.PathValue("channel")
	if !uuidRe.MatchString(chID) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	partial := isHTMX(r) && r.Header.Get("HX-Target") == "main" && r.Header.Get("HX-History-Restore-Request") != "true"
	if partial {
		team, err := h.team(ctx, r.PathValue("team"))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		view, err := h.channelView(ctx, u, team, chID, msgParam(r))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		view.OpenThread = threadParam(r)
		h.fragment(w, r, http.StatusOK, "channel", view)
		return
	}

	var (
		teams []*pb.Team
		team  *pb.Team
		lists *render.Lists
		view  *render.ChannelView
	)
	res, err := h.client.Teams.ListTeams(ctx, &pb.ListTeamsRequest{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	teams = res.Teams
	for _, t := range teams {
		if t.Name == r.PathValue("team") {
			team = t
		}
	}
	if team == nil {
		h.notFound(w, r)
		return
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		lists, err = h.lists(gctx, u, team, chID)
		return err
	})
	g.Go(func() error {
		var err error
		view, err = h.channelView(gctx, u, team, chID, msgParam(r))
		return err
	})
	if err := g.Wait(); err != nil {
		h.fail(w, r, err)
		return
	}
	view.OpenThread = threadParam(r)
	h.page(w, r, http.StatusOK, "app", appPage{
		Page:    Page{Title: view.Title, CSRF: h.csrfToken(w, r), User: u},
		Teams:   teams,
		Team:    team,
		Lists:   lists,
		Channel: view,
	})
}

// lists loads the sidebar of a page for u, and keeps it for the page's
// socket.
func (h *Handler) lists(ctx context.Context, u *pb.User, team *pb.Team, active string) (*render.Lists, error) {
	res, err := h.client.Channels.ListChannels(ctx, &pb.ListChannelsRequest{TeamId: team.Id})
	if err != nil {
		return nil, err
	}
	var unread int
	if t, err := h.client.Messages.ListThreads(ctx, &pb.ListThreadsRequest{TeamId: team.Id}); err == nil {
		unread = int(t.UnreadCount)
	}
	channels, voice, direct := render.Split(res.Channels)
	var status map[string]pb.Status
	if peers := render.Peers(direct); len(peers) > 0 {
		if p, err := h.client.Users.GetPresence(ctx, &pb.GetPresenceRequest{UserIds: peers}); err == nil {
			status = p.Statuses
		}
	}
	lists := &render.Lists{Team: team, Channels: channels, Voice: voice, DMs: direct, Active: active, Status: status, ThreadsUnread: unread}
	// Who talks where is a nicety: the page shows without it.
	if len(voice) > 0 {
		if vs, err := h.client.Voice.ListVoiceStates(ctx, &pb.ListVoiceStatesRequest{TeamId: team.Id}); err == nil {
			lists.VoiceStates = render.VoiceStates(vs.Channels)
		}
	}
	if h.gw.Sidebars != nil {
		lists.Key = h.gw.Sidebars.Put(gateway.NewSidebar(u.Id, lists))
	}
	return lists, nil
}

func (h *Handler) olderMessages(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	chID, before := r.PathValue("channel"), r.URL.Query().Get("before")
	if !uuidRe.MatchString(chID) || !uuidRe.MatchString(before) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	var (
		team *pb.Team
		ch   *pb.Channel
		msgs *pb.ListMessagesResponse
	)
	g.Go(func() (err error) { team, err = h.team(gctx, r.PathValue("team")); return })
	g.Go(func() error {
		res, err := h.client.Channels.GetChannel(gctx, &pb.GetChannelRequest{ChannelId: chID})
		if err == nil {
			ch = res.Channel
		}
		return err
	})
	g.Go(func() (err error) {
		msgs, err = h.client.Messages.ListMessages(gctx, &pb.ListMessagesRequest{ChannelId: chID, BeforeId: before, Limit: pageSize})
		return
	})
	if err := g.Wait(); err != nil {
		h.fail(w, r, err)
		return
	}
	mod := moderator(u, team, ch)
	view := &render.ChannelView{Team: team, Channel: ch, Title: render.Title(ch), Member: true, Moderator: mod, HasMore: msgs.HasMore,
		Messages: h.render.Messages(msgs.Messages, u, mod)}
	if n := len(msgs.Messages); n > 0 {
		view.Oldest = msgs.Messages[n-1].Id
	}
	h.fragment(w, r, http.StatusOK, "older-page", view)
}

func (h *Handler) teamFor(w http.ResponseWriter, r *http.Request) (*pb.User, *pb.Team, context.Context, context.CancelFunc) {
	u := h.signedIn(w, r)
	if u == nil {
		return nil, nil, nil, nil
	}
	ctx, cancel := h.ctx(r)
	team, err := h.team(ctx, r.PathValue("team"))
	if err != nil {
		cancel()
		h.fail(w, r, err)
		return nil, nil, nil, nil
	}
	return u, team, ctx, cancel
}

func (h *Handler) newChannelModal(w http.ResponseWriter, r *http.Request) {
	_, team, _, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	h.fragment(w, r, http.StatusOK, "new-channel", struct {
		Team  *pb.Team
		Voice bool
	}{team, r.URL.Query().Get("type") == "voice"})
}

func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	kind := pb.ChannelType_CHANNEL_TYPE_PUBLIC
	switch r.PostFormValue("type") {
	case "private":
		kind = pb.ChannelType_CHANNEL_TYPE_PRIVATE
	case "voice":
		kind = pb.ChannelType_CHANNEL_TYPE_VOICE
	}
	res, err := h.client.Channels.CreateChannel(ctx, &pb.CreateChannelRequest{
		TeamId: team.Id, Name: r.PostFormValue("name"), DisplayName: r.PostFormValue("display_name"),
		Purpose: r.PostFormValue("purpose"), Type: kind, VoiceDisabled: r.PostFormValue("voice") != "1",
	})
	if err != nil {
		if msg, ok := formError(err); ok {
			h.text(w, r, http.StatusOK, "error", msg)
			return
		}
		h.fail(w, r, err)
		return
	}
	h.navigate(w, r, "/t/"+team.Name+"/ch/"+res.Channel.Id, true)
}

func (h *Handler) browseModal(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	res, err := h.client.Channels.BrowseChannels(ctx, &pb.BrowseChannelsRequest{TeamId: team.Id})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "browse", struct {
		Team               *pb.Team
		Channels, Archived []*pb.Channel
	}{team, res.Channels, res.Archived})
}

func (h *Handler) channelTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	chID := r.PathValue("channel")
	if !uuidRe.MatchString(chID) {
		h.notFound(w, r)
		return "", false
	}
	return chID, true
}

func (h *Handler) joinChannel(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID, ok := h.channelTarget(w, r)
	if !ok {
		return
	}
	if _, err := h.client.Channels.JoinChannel(ctx, &pb.JoinChannelRequest{ChannelId: chID}); err != nil {
		h.fail(w, r, err)
		return
	}
	h.navigate(w, r, "/t/"+team.Name+"/ch/"+chID, true)
}

func (h *Handler) leaveChannel(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID, ok := h.channelTarget(w, r)
	if !ok {
		return
	}
	if _, err := h.client.Channels.LeaveChannel(ctx, &pb.LeaveChannelRequest{ChannelId: chID}); err != nil {
		h.fail(w, r, err)
		return
	}
	next, err := h.defaultChannel(ctx, team)
	if err != nil {
		h.reload(w, r, "/")
		return
	}
	h.navigate(w, r, "/t/"+team.Name+"/ch/"+next, true)
}

type peopleData struct {
	Heading     string
	SearchURL   string
	ActionURL   string
	ActionLabel string
	Members     []*pb.TeamMember
}

func (h *Handler) searchPeople(ctx context.Context, team *pb.Team, query, exclude string) ([]*pb.TeamMember, error) {
	if len(query) > 64 {
		query = query[:64]
	}
	res, err := h.client.Teams.ListTeamMembers(ctx, &pb.ListTeamMembersRequest{TeamId: team.Id, Query: query, Limit: 25})
	if err != nil {
		return nil, err
	}
	out := res.Members[:0]
	for _, m := range res.Members {
		if m.User.Id != exclude {
			out = append(out, m)
		}
	}
	return out, nil
}

func (h *Handler) peopleModal(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	members, err := h.searchPeople(ctx, team, "", u.Id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "dm-picker", peopleData{SearchURL: "/t/" + team.Name + "/people/search", ActionURL: "/t/" + team.Name + "/dm", Members: members})
}

func (h *Handler) peopleSearch(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	members, err := h.searchPeople(ctx, team, r.URL.Query().Get("q"), u.Id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "dm-results", peopleData{Members: members})
}

// maxPicked bounds the people a conversation is started with.
const maxPicked = 7

// openDirect opens the conversation with the people picked: one for a
// direct message, more for a group.
func (h *Handler) openDirect(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	_ = r.ParseForm()
	picked := r.PostForm["user_id"]
	if len(picked) == 0 || len(picked) > maxPicked {
		h.fail(w, r, ierrors.InvalidArgument("pick one to seven people"))
		return
	}
	res, err := h.client.Channels.OpenGroupChannel(ctx, &pb.OpenGroupChannelRequest{TeamId: team.Id, UserIds: picked})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.navigate(w, r, "/t/"+team.Name+"/ch/"+res.Channel.Id, true)
}

type inviteListData struct {
	Team    *pb.Team
	Invites []*pb.InviteSummary
	MoreURL string
	OOB     bool
}

func (h *Handler) invites(ctx context.Context, team *pb.Team, after string) (inviteListData, error) {
	res, err := h.client.Teams.ListInvites(ctx, &pb.ListInvitesRequest{TeamId: team.Id, After: after})
	if err != nil {
		return inviteListData{}, err
	}
	data := inviteListData{Team: team, Invites: res.Invites}
	if res.Next != "" {
		data.MoreURL = "/t/" + team.Name + "/invites?" + url.Values{"after": {res.Next}}.Encode()
	}
	return data, nil
}

// moreInvites continues the list of open invites.
func (h *Handler) moreInvites(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	data, err := h.invites(ctx, team, r.URL.Query().Get("after"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "invite-rows", data)
}

func (h *Handler) inviteModal(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	data, err := h.invites(ctx, team, "")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "invite", data)
}

// createInvite shows the new link, once, and refreshes the open invites.
func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	res, err := h.client.Teams.CreateInvite(ctx, &pb.CreateInviteRequest{TeamId: team.Id})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var out strings.Builder
	err = h.render.Fragment(&out, "invite-link", struct {
		URL       string
		ExpiresAt time.Time
	}{h.cfg.PublicURL + "/join/" + res.Token, res.ExpiresAt.AsTime()})
	if err == nil {
		if data, lerr := h.invites(ctx, team, ""); lerr == nil {
			data.OOB = true
			err = h.render.Fragment(&out, "invite-list", data)
		}
	}
	if err != nil {
		h.fail(w, r, ierrors.Internal(err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, out.String())
}

// revokeInvite answers with the open invites, and why when the invite could
// not be revoked.
func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	_, err := h.client.Teams.RevokeInvite(ctx, &pb.RevokeInviteRequest{TeamId: team.Id, InviteId: r.PathValue("invite")})
	if err != nil {
		if code := drpc.Err(err).Code; code != ierrors.CodeFailedPrecondition && code != ierrors.CodeNotFound {
			h.fail(w, r, err)
			return
		}
	}
	data, lerr := h.invites(ctx, team, "")
	if lerr != nil {
		h.fail(w, r, lerr)
		return
	}
	var out strings.Builder
	rerr := h.render.Fragment(&out, "invite-list", data)
	if rerr == nil && err != nil {
		rerr = h.render.Fragment(&out, "toast", render.Toast{Kind: "error", Text: drpc.Err(err).Message})
	}
	if rerr != nil {
		h.fail(w, r, ierrors.Internal(rerr))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, out.String())
}

type teamMembersData struct {
	Team    *pb.Team
	Admin   bool
	System  bool
	MeID    string
	Members []*pb.TeamMember
	MoreURL string
}

// CanChange reports whether the viewer may change m's role or remove them,
// as the server allows: admins only the owner may, the owner no one else.
func (d teamMembersData) CanChange(m *pb.TeamMember) bool {
	id := m.GetUser().GetId()
	switch {
	case !d.Admin || id == d.MeID:
		return false
	case d.System:
		return true
	case id == d.Team.GetOwnerId():
		return false
	case m.Role == pb.TeamRole_TEAM_ROLE_ADMIN:
		return d.MeID == d.Team.GetOwnerId()
	}
	return true
}

// memberPage is how many members the team's list shows at once.
const memberPage = 100

// renderTeamMembers shows the team's members, or with after the next of
// them.
func (h *Handler) renderTeamMembers(w http.ResponseWriter, r *http.Request, ctx context.Context, u *pb.User, team *pb.Team, after string) {
	res, err := h.client.Teams.ListTeamMembers(ctx, &pb.ListTeamMembersRequest{TeamId: team.Id, Limit: memberPage, After: after})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	data := teamMembersData{
		Team: team, Admin: team.Role == pb.TeamRole_TEAM_ROLE_ADMIN || u.Role == pb.SystemRole_SYSTEM_ROLE_ADMIN,
		System: u.Role == pb.SystemRole_SYSTEM_ROLE_ADMIN, MeID: u.Id, Members: res.Members,
	}
	if res.Next != "" {
		data.MoreURL = "/t/" + team.Name + "/members?" + url.Values{"after": {res.Next}}.Encode()
	}
	name := "team-members"
	if after != "" {
		name = "team-member-rows"
	}
	h.fragment(w, r, http.StatusOK, name, data)
}

func (h *Handler) teamMembers(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	after := r.URL.Query().Get("after")
	if len(after) > 64 {
		h.fail(w, r, ierrors.InvalidArgument("invalid cursor"))
		return
	}
	h.renderTeamMembers(w, r, ctx, u, team, after)
}

func (h *Handler) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	if _, err := h.client.Teams.RemoveTeamMember(ctx, &pb.RemoveTeamMemberRequest{TeamId: team.Id, UserId: r.PathValue("user")}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// leaveTeam takes the caller out of the team, then home; a refusal shows in
// the team's settings.
func (h *Handler) leaveTeam(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	if _, err := h.client.Teams.LeaveTeam(ctx, &pb.LeaveTeamRequest{TeamId: team.Id}); err != nil {
		msg, ok := formError(err)
		if !ok {
			h.fail(w, r, err)
			return
		}
		h.fragment(w, r, http.StatusOK, "leave-team", teamSettingsData{Team: team, Error: msg})
		return
	}
	h.reload(w, r, "/")
}

func (h *Handler) teamMemberRole(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	role := pb.TeamRole_TEAM_ROLE_MEMBER
	if r.PostFormValue("role") == "admin" {
		role = pb.TeamRole_TEAM_ROLE_ADMIN
	}
	if _, err := h.client.Teams.UpdateTeamMemberRole(ctx, &pb.UpdateTeamMemberRoleRequest{TeamId: team.Id, UserId: r.PathValue("user"), Role: role}); err != nil {
		h.fail(w, r, err)
		return
	}
	h.renderTeamMembers(w, r, ctx, u, team, "")
}

type channelMembersData struct {
	Team      *pb.Team
	ChannelID string
	CanAdd    bool
	CanRemove bool
	Members   []*pb.ChannelMember
}

func (h *Handler) renderChannelMembers(w http.ResponseWriter, r *http.Request, ctx context.Context, u *pb.User, team *pb.Team, chID string) {
	g, gctx := errgroup.WithContext(ctx)
	var (
		ch      *pb.Channel
		members []*pb.ChannelMember
	)
	g.Go(func() error {
		res, err := h.client.Channels.GetChannel(gctx, &pb.GetChannelRequest{ChannelId: chID})
		if err == nil {
			ch = res.Channel
		}
		return err
	})
	g.Go(func() error {
		res, err := h.client.Channels.ListChannelMembers(gctx, &pb.ListChannelMembersRequest{ChannelId: chID})
		if err == nil {
			members = res.Members
		}
		return err
	})
	if err := g.Wait(); err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "channel-members", channelMembersData{
		Team: team, ChannelID: chID, Members: members,
		CanAdd:    !render.Conversation(ch),
		CanRemove: !render.Conversation(ch) && moderator(u, team, ch),
	})
}

func (h *Handler) channelMembers(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID, ok := h.channelTarget(w, r)
	if !ok {
		return
	}
	h.renderChannelMembers(w, r, ctx, u, team, chID)
}

func (h *Handler) addMemberModal(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID, ok := h.channelTarget(w, r)
	if !ok {
		return
	}
	members, err := h.searchPeople(ctx, team, "", u.Id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	base := "/t/" + team.Name + "/ch/" + chID
	h.fragment(w, r, http.StatusOK, "people", peopleData{
		Heading: "Add people", SearchURL: base + "/add/search", ActionURL: base + "/members", ActionLabel: "Add", Members: members,
	})
}

func (h *Handler) addMemberSearch(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID, ok := h.channelTarget(w, r)
	if !ok {
		return
	}
	members, err := h.searchPeople(ctx, team, r.URL.Query().Get("q"), u.Id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "people-results", peopleData{
		ActionURL: "/t/" + team.Name + "/ch/" + chID + "/members", ActionLabel: "Add", Members: members,
	})
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	u, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID, ok := h.channelTarget(w, r)
	if !ok {
		return
	}
	if _, err := h.client.Channels.AddChannelMember(ctx, &pb.AddChannelMemberRequest{ChannelId: chID, UserId: r.PostFormValue("user_id")}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Retarget", "#modal")
	w.Header().Set("HX-Reswap", "innerHTML")
	h.renderChannelMembers(w, r, ctx, u, team, chID)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID, ok := h.channelTarget(w, r)
	if !ok {
		return
	}
	if _, err := h.client.Channels.RemoveChannelMember(ctx, &pb.RemoveChannelMemberRequest{ChannelId: chID, UserId: r.PathValue("user")}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// messageTarget reads a message's channel and id, and for a reply the id of
// its thread's root from the query.
func (h *Handler) messageTarget(w http.ResponseWriter, r *http.Request) (string, string, string, bool) {
	chID, msgID, rootID := r.PathValue("channel"), r.PathValue("message"), r.URL.Query().Get("root")
	if !uuidRe.MatchString(chID) || !uuidRe.MatchString(msgID) || (rootID != "" && !uuidRe.MatchString(rootID)) {
		h.notFound(w, r)
		return "", "", "", false
	}
	return chID, msgID, rootID, true
}

func (h *Handler) loadMessage(w http.ResponseWriter, r *http.Request) (*pb.User, *pb.Message) {
	u := h.signedIn(w, r)
	if u == nil {
		return nil, nil
	}
	chID, msgID, rootID, ok := h.messageTarget(w, r)
	if !ok {
		return nil, nil
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Messages.GetMessage(ctx, &pb.GetMessageRequest{ChannelId: chID, MessageId: msgID, RootId: rootID})
	if err != nil {
		h.fail(w, r, err)
		return nil, nil
	}
	return u, res.Message
}

func (h *Handler) messageFragment(w http.ResponseWriter, r *http.Request) {
	u, m := h.loadMessage(w, r)
	if m == nil {
		return
	}
	h.fragment(w, r, http.StatusOK, "message", h.render.Message(m, u, false))
}

func (h *Handler) editMessageForm(w http.ResponseWriter, r *http.Request) {
	u, m := h.loadMessage(w, r)
	if m == nil {
		return
	}
	if m.GetAuthor().GetId() != u.Id {
		h.fail(w, r, ierrors.PermissionDenied())
		return
	}
	h.fragment(w, r, http.StatusOK, "message-edit", m)
}

func (h *Handler) editMessage(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	chID, msgID, rootID, ok := h.messageTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Messages.EditMessage(ctx, &pb.EditMessageRequest{ChannelId: chID, MessageId: msgID, RootId: rootID, Content: r.PostFormValue("content")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "message", h.render.Message(res.Message, u, false))
}

// removeLinkPreview takes the preview off the caller's message.
func (h *Handler) removeLinkPreview(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	chID, msgID, rootID, ok := h.messageTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Messages.RemoveLinkPreview(ctx, &pb.RemoveLinkPreviewRequest{ChannelId: chID, MessageId: msgID, RootId: rootID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "message", h.render.Message(res.Message, u, false))
}

func (h *Handler) deleteMessage(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	chID, msgID, rootID, ok := h.messageTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	if _, err := h.client.Messages.DeleteMessage(ctx, &pb.DeleteMessageRequest{ChannelId: chID, MessageId: msgID, RootId: rootID}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

type threadView struct {
	Root      *render.Message
	Replies   []*render.Message
	HasMore   bool
	Oldest    string
	Title     string
	Member    bool
	Following bool
	Voice     bool
}

// threadPanel shows a thread, or with ?before its older replies.
func (h *Handler) threadPanel(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	chID, rootID, before := r.PathValue("channel"), r.PathValue("message"), r.URL.Query().Get("before")
	if !uuidRe.MatchString(chID) || !uuidRe.MatchString(rootID) || (before != "" && !uuidRe.MatchString(before)) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	var (
		ch     *pb.Channel
		team   *pb.Team
		thread *pb.ListThreadResponse
	)
	g.Go(func() error {
		res, err := h.client.Channels.GetChannel(gctx, &pb.GetChannelRequest{ChannelId: chID})
		if err != nil {
			return err
		}
		ch = res.Channel
		if ch.TeamId != "" {
			t, err := h.client.Teams.GetTeam(gctx, &pb.GetTeamRequest{Key: &pb.GetTeamRequest_Id{Id: ch.TeamId}})
			if err != nil {
				return err
			}
			team = t.Team
		}
		return nil
	})
	g.Go(func() (err error) {
		thread, err = h.client.Messages.ListThread(gctx, &pb.ListThreadRequest{ChannelId: chID, RootId: rootID, BeforeId: before, Limit: pageSize})
		return
	})
	if err := g.Wait(); err != nil {
		h.fail(w, r, err)
		return
	}
	mod := moderator(u, team, ch)
	view := threadView{
		Root:      h.render.Message(thread.Root, u, mod),
		Replies:   h.render.Messages(thread.Replies, u, mod),
		HasMore:   thread.HasMore,
		Title:     render.Title(ch),
		Member:    ch.Role != pb.ChannelRole_CHANNEL_ROLE_UNSPECIFIED,
		Following: thread.Following,
		Voice:     ch.VoiceEnabled,
	}
	if len(thread.Replies) > 0 {
		view.Oldest = thread.Replies[0].Id
	}
	if before != "" {
		h.fragment(w, r, http.StatusOK, "older-replies-page", view)
		return
	}
	if thread.Following {
		if _, err := h.client.Messages.MarkThreadRead(ctx, &pb.MarkThreadReadRequest{ChannelId: chID, RootId: rootID}); err != nil {
			h.logger.Debug("failed to mark a thread read", zap.Error(err))
		}
	}
	h.fragment(w, r, http.StatusOK, "thread", view)
}

type suggestion struct {
	Name string
	User *pb.User
	Note string
}

var (
	mentionPrefixRe = regexp.MustCompile(`^[a-z0-9._-]{0,32}$`)
	everyone        = []suggestion{
		{Name: "channel", Note: "Everyone in this channel"},
		{Name: "all", Note: "Everyone in this channel"},
		{Name: "here", Note: "Everyone online in this channel"},
	}
)

const maxSuggestions = 8

// mentionSuggestions lists the channel's members whose name starts with q,
// for the composer's @ completion.
func (h *Handler) mentionSuggestions(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	chID, q := r.PathValue("channel"), strings.ToLower(r.URL.Query().Get("q"))
	if !uuidRe.MatchString(chID) || !mentionPrefixRe.MatchString(q) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	var (
		ch      *pb.Channel
		members []*pb.ChannelMember
	)
	g.Go(func() error {
		res, err := h.client.Channels.GetChannel(gctx, &pb.GetChannelRequest{ChannelId: chID})
		if err == nil {
			ch = res.Channel
		}
		return err
	})
	g.Go(func() error {
		res, err := h.client.Channels.ListChannelMembers(gctx, &pb.ListChannelMembersRequest{ChannelId: chID})
		if err == nil {
			members = res.Members
		}
		return err
	})
	if err := g.Wait(); err != nil {
		h.fail(w, r, err)
		return
	}
	slices.SortFunc(members, func(a, b *pb.ChannelMember) int {
		return cmp.Compare(a.GetUser().GetUsername(), b.GetUser().GetUsername())
	})
	var items []suggestion
	for _, m := range members {
		u := m.GetUser()
		if u == nil || len(items) == maxSuggestions {
			continue
		}
		if strings.HasPrefix(u.Username, q) || strings.HasPrefix(strings.ToLower(u.DisplayName), q) {
			items = append(items, suggestion{Name: u.Username, User: u})
		}
	}
	if !render.Conversation(ch) {
		for _, e := range everyone {
			if strings.HasPrefix(e.Name, q) && len(items) < maxSuggestions {
				items = append(items, e)
			}
		}
	}
	h.fragment(w, r, http.StatusOK, "suggest", items)
}

func (h *Handler) reactionPicker(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	chID, msgID, rootID, ok := h.messageTarget(w, r)
	if !ok {
		return
	}
	h.fragment(w, r, http.StatusOK, "emoji-picker", struct {
		URL   string
		Emoji []emoji.Emoji
	}{render.ReactURL(chID, msgID, rootID), emoji.All})
}

// react adds or takes back a reaction and answers with the message's
// reaction bar; a choice from the picker also closes it.
func (h *Handler) react(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	chID, msgID, rootID, ok := h.messageTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Messages.React(ctx, &pb.ReactRequest{
		ChannelId: chID, MessageId: msgID, RootId: rootID, Emoji: r.PostFormValue("emoji"), Remove: r.PostFormValue("remove") == "1",
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if r.PostFormValue("picker") == "1" {
		w.Header().Set("HX-Trigger", "closeModal")
	}
	h.fragment(w, r, http.StatusOK, "reactions", render.Reactions(chID, msgID, rootID, res.Reactions, u, true))
}

// threadParam is the valid ?thread of a request, or empty.
func threadParam(r *http.Request) string {
	if t := r.URL.Query().Get("thread"); uuidRe.MatchString(t) {
		return t
	}
	return ""
}

// channelLink sends /c/{channel} to the channel in its team, for links that
// do not know the team, such as a notification's.
func (h *Handler) channelLink(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	chID := r.PathValue("channel")
	if !uuidRe.MatchString(chID) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Channels.GetChannel(ctx, &pb.GetChannelRequest{ChannelId: chID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var name string
	if id := res.Channel.TeamId; id != "" {
		t, err := h.client.Teams.GetTeam(ctx, &pb.GetTeamRequest{Key: &pb.GetTeamRequest_Id{Id: id}})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		name = t.Team.Name
	} else {
		teams, err := h.client.Teams.ListTeams(ctx, &pb.ListTeamsRequest{})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if len(teams.Teams) == 0 {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		name = teams.Teams[0].Name
	}
	target := "/t/" + name + "/ch/" + chID
	if t := threadParam(r); t != "" {
		target += "?thread=" + t
	}
	http.Redirect(w, r, safeNext(target), http.StatusSeeOther) //nolint:gosec // safeNext keeps it on this site
}

func (h *Handler) muteChannel(w http.ResponseWriter, r *http.Request) {
	_, team, ctx, cancel := h.teamFor(w, r)
	if team == nil {
		return
	}
	defer cancel()
	chID := r.PathValue("channel")
	if !uuidRe.MatchString(chID) {
		h.notFound(w, r)
		return
	}
	muted := r.PostFormValue("muted") == "1"
	if _, err := h.client.Channels.SetChannelMuted(ctx, &pb.SetChannelMutedRequest{ChannelId: chID, Muted: muted}); err != nil {
		h.fail(w, r, err)
		return
	}
	h.fragment(w, r, http.StatusOK, "mute-button", map[string]any{"Team": team.Name, "Channel": chID, "Muted": muted})
}
