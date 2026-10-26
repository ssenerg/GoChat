// Package directory answers who belongs where: teams and their one-time
// invites, the channels a person sees, direct conversations and profiles.
// It runs as a pool of workers; membership changes of an existing channel go
// through that channel's actor.
package directory

import (
	"cmp"
	"context"
	"encoding/hex"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/keylock"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	"github.com/ssenerg/gochat/internal/core/validate"
	"github.com/ssenerg/gochat/internal/core/window"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/invites"
	"github.com/ssenerg/gochat/internal/services/updates"
	"github.com/ssenerg/gochat/internal/services/users"
	"github.com/ssenerg/gochat/internal/services/voice"
)

type Repo interface {
	GetUser(ctx context.Context, id gocql.UUID) (*models.User, error)
	ChannelsPts(ctx context.Context, channelIDs []gocql.UUID) (map[gocql.UUID]int64, error)
	ListBotIDs(ctx context.Context, owner gocql.UUID) ([]gocql.UUID, error)
	UpdateDisplayName(ctx context.Context, id gocql.UUID, name string, now time.Time) error
	UpdateMemberDisplayName(ctx context.Context, userID gocql.UUID, name string) error
	SetUserPhoto(ctx context.Context, id gocql.UUID, version string, now time.Time) error
	SetStatus(ctx context.Context, id gocql.UUID, status string, dndUntil time.Time) error
	SetCustomStatus(ctx context.Context, id gocql.UUID, emoji, text string, until time.Time) error

	CreateTeam(ctx context.Context, n *scylla.NewTeam) error
	GetTeam(ctx context.Context, id gocql.UUID) (*models.Team, error)
	GetTeamIDByName(ctx context.Context, name string) (gocql.UUID, error)
	GetTeams(ctx context.Context, ids []gocql.UUID) ([]*models.Team, error)
	SetTeamPhoto(ctx context.Context, id gocql.UUID, version string) error
	AddTeamMember(ctx context.Context, m *models.TeamMember, username, displayName string) error
	GetTeamMember(ctx context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error)
	ListTeamMembers(ctx context.Context, teamID gocql.UUID) ([]*scylla.TeamMemberEntry, error)
	ListUserTeams(ctx context.Context, userID gocql.UUID) ([]*models.TeamMember, error)
	RemoveTeamMember(ctx context.Context, teamID, userID gocql.UUID) error
	UpdateTeamMemberRole(ctx context.Context, teamID, userID gocql.UUID, role models.TeamRole) error
	CreateInvite(ctx context.Context, inv *models.Invite) error
	ListTeamInvites(ctx context.Context, teamID gocql.UUID) ([]*models.Invite, error)
	FindTeamInvites(ctx context.Context, teamID gocql.UUID, prefix []byte) ([]*models.Invite, error)
	OpenInvites(ctx context.Context, teamID gocql.UUID, listed []*models.Invite) ([]*models.Invite, error)
	RevokeInvite(ctx context.Context, teamID gocql.UUID, hash []byte) (bool, error)
	GetInvite(ctx context.Context, hash []byte) (*models.Invite, error)
	ConsumeInvite(ctx context.Context, inv *models.Invite, userID gocql.UUID, now time.Time) (bool, error)
	ReleaseInvite(ctx context.Context, inv *models.Invite, userID gocql.UUID, now time.Time) error
	SpendInvite(ctx context.Context, hash []byte, userID gocql.UUID) error

	CreateChannel(ctx context.Context, ch *models.Channel, creator *models.ChannelMember) error
	CreateDirectChannel(ctx context.Context, key string, ch *models.Channel, members []*models.ChannelMember) (gocql.UUID, error)
	ClaimDirectKey(ctx context.Context, key string, id gocql.UUID) (gocql.UUID, error)
	ChannelsAfter(ctx context.Context, after gocql.UUID, limit int) ([]*models.Channel, error)
	ListChannelMembers(ctx context.Context, channelID gocql.UUID) ([]*models.ChannelMember, error)
	GetState(ctx context.Context, key string) (string, error)
	SetState(ctx context.Context, key, value string) error
	GetChannels(ctx context.Context, ids []gocql.UUID) ([]*models.Channel, error)
	GetChannelIDByName(ctx context.Context, teamID gocql.UUID, name string) (gocql.UUID, error)
	ListTeamChannels(ctx context.Context, teamID gocql.UUID) ([]*scylla.TeamChannel, error)
	ListUserChannels(ctx context.Context, userID, teamID gocql.UUID) ([]*models.ChannelMember, error)
	ListLastReads(ctx context.Context, userID gocql.UUID) ([]*scylla.ChannelRead, error)
	CountMentions(ctx context.Context, userID gocql.UUID) (map[gocql.UUID]int, error)
	ListMuted(ctx context.Context, userID gocql.UUID) (map[gocql.UUID]bool, error)
	ListCleared(ctx context.Context, userID gocql.UUID) (map[gocql.UUID]gocql.UUID, error)
	ListThreadFollows(ctx context.Context, userID gocql.UUID, limit int) ([]*scylla.ThreadFollow, error)
	GetMessage(ctx context.Context, channelID, id gocql.UUID) (*models.Message, error)
	GetReply(ctx context.Context, channelID, rootID, id gocql.UUID) (*models.Message, error)
	ListSaved(ctx context.Context, userID gocql.UUID, limit int) ([]*scylla.Saved, error)
}

type Deps struct {
	Logger   *zap.Logger
	Cfg      *Config
	Repo     Repo
	Storage  Storage
	Authz    *authz.Authorizer
	Users    *users.Cache
	Engine   *actor.Engine
	Hub      *actor.PID
	Channels *actor.PID
	Voice    *actor.PID
	// Updates numbers what happens to each account.
	Updates *actor.PID
	invited *window.Counter
	created *window.Counter
	// teamLocks serializes the changes that could leave a team without an
	// admin.
	teamLocks *keylock.Locks
}

// The channels every team starts with and every member joins.
var defaultChannels = []struct{ name, display, purpose string }{
	{channels.DefaultChannel, "Town Square", "Everyone in the team"},
	{"off-topic", "Off-Topic", "Anything that is not work"},
}

func Pool(d *Deps) actor.Producer {
	d.invited = window.New(d.Cfg.MaxInvites, d.Cfg.InviteTTL)
	d.created = window.New(d.Cfg.MaxTeamsPerDay, 24*time.Hour)
	d.teamLocks = keylock.New()
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
	case CreateTeam:
		out, err = w.createTeam(ctx, msg)
	case ListTeams:
		out, err = w.listTeams(ctx, msg.P)
	case GetTeam:
		out, err = w.getTeam(ctx, msg)
	case ListTeamMembers:
		out, err = w.listTeamMembers(ctx, msg)
	case RemoveTeamMember:
		out, err = w.removeTeamMember(ctx, msg)
	case AddBot:
		out, err = w.addBot(ctx, msg)
	case RemoveBot:
		out, err = w.removeBot(ctx, msg)
	case UpdateTeamMemberRole:
		out, err = w.updateTeamMemberRole(ctx, msg)
	case CreateInvite:
		out, err = w.createInvite(ctx, msg)
	case ListInvites:
		out, err = w.listInvites(ctx, msg)
	case RevokeInvite:
		out, err = w.revokeInvite(ctx, msg)
	case GetInvite:
		out, err = w.getInvite(ctx, msg.Token)
	case AcceptInvite:
		out, err = w.acceptInvite(ctx, msg)
	case JoinTeam:
		out, err = w.joinTeam(ctx, msg.UserID, msg.TeamID)
		if err == nil && msg.InviteHash != nil {
			w.spendInvite(ctx, msg.InviteHash, msg.UserID)
		}
	case ListChannels:
		out, err = w.listChannels(ctx, msg)
	case BrowseChannels:
		out, err = w.browseChannels(ctx, msg)
	case CreateChannel:
		out, err = w.createChannel(ctx, msg)
	case OpenDirect:
		out, err = w.openDirect(ctx, msg)
	case OpenGroup:
		out, err = w.openGroup(ctx, msg)
	case ListThreads:
		out, err = w.listThreads(ctx, msg)
	case ListSaved:
		out, err = w.listSaved(ctx, msg)
	case GetUser:
		out, err = w.getUser(ctx, msg)
	case UpdateProfile:
		out, err = w.updateProfile(ctx, msg)
	case SetPhoto:
		out, err = w.setPhoto(ctx, msg.P, msg.Image)
	case SetTeamPhoto:
		out, err = w.setTeamPhoto(ctx, msg.P, msg.TeamID, msg.Image)
	case GetPhoto:
		out, err = w.getPhoto(ctx, msg)
	case Me:
		out, err = w.me(ctx, msg.P)
	case Presence:
		out, err = w.presence(ctx, msg)
	case Acquainted:
		out, err = w.acquaintances(ctx, msg)
	case SetStatus:
		out, err = w.setStatus(ctx, msg)
	case SetCustomStatus:
		out, err = w.setCustomStatus(ctx, msg)
	case ListMuted:
		out, err = w.listMuted(ctx, msg.P)
	default:
		return
	}
	if err != nil {
		if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
			w.Logger.Error("directory request failed", e.Fields()...)
		}
		actors.Reply(c, err)
		return
	}
	actors.Reply(c, out)
}

func parseID(s, what string) (gocql.UUID, error) {
	id, ok := ids.Parse(s)
	if !ok {
		return gocql.UUID{}, ierrors.InvalidArgument("invalid " + what + " id")
	}
	return id, nil
}

var errNoTeam = ierrors.NotFound("team not found")

// membership returns the caller's membership of teamID, hiding teams the
// caller does not belong to.
func (w *worker) membership(ctx context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error) {
	m, err := w.Repo.GetTeamMember(ctx, teamID, userID)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, errNoTeam
		}
		return nil, err
	}
	return m, nil
}

// deliver numbers ev in the updates of each of userIDs and sends it to
// them.
func (w *worker) deliver(ev *pb.Event, userIDs ...gocql.UUID) {
	if w.Updates == nil {
		to := make([]string, len(userIDs))
		for i, id := range userIDs {
			to[i] = id.String()
		}
		w.Engine.Send(w.Hub, hub.Deliver{UserIDs: to, Event: ev})
		return
	}
	w.Engine.Send(w.Updates, updates.Push{UserIDs: userIDs, Event: ev})
}

func (w *worker) createTeam(ctx context.Context, msg CreateTeam) (*pb.Team, error) {
	if !w.Authz.Can(authz.ObjTeam, authz.ActCreate, string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	name, err := validate.Slug(msg.Name)
	if err != nil {
		return nil, err
	}
	display, err := validate.Line("display name", msg.DisplayName, 1, 64)
	if err != nil {
		return nil, err
	}
	desc, err := validate.Line("description", msg.Description, 0, 250)
	if err != nil {
		return nil, err
	}
	key := msg.P.UserID.String()
	limited := msg.P.Role != models.SystemRoleAdmin
	if limited && w.created.Blocked(key) {
		return nil, ierrors.ResourceExhausted("you created too many teams today, try again tomorrow")
	}
	creator, err := w.Users.Get(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	team := &models.Team{ID: ids.New(), Name: name, DisplayName: display, Description: desc, CreatedBy: creator.ID, CreatedAt: now}
	n := &scylla.NewTeam{
		Team:     team,
		Creator:  &models.TeamMember{TeamID: team.ID, UserID: creator.ID, Role: models.TeamRoleAdmin, JoinedAt: now},
		Username: creator.Username, DisplayName: creator.DisplayName,
	}
	for _, dc := range defaultChannels {
		n.Channels = append(n.Channels, &models.Channel{
			ID: ids.New(), TeamID: team.ID, Name: dc.name, DisplayName: dc.display, Purpose: dc.purpose,
			Kind: models.ChannelPublic, CreatedBy: creator.ID, CreatedAt: now,
		})
	}
	if err := w.Repo.CreateTeam(ctx, n); err != nil {
		return nil, err
	}
	if limited {
		w.created.Add(key)
	}
	out := convert.Team(team, models.TeamRoleAdmin)
	w.deliver(&pb.Event{Payload: &pb.Event_TeamJoined{TeamJoined: &pb.TeamJoined{Team: out}}}, creator.ID)
	w.Logger.Info("team created", zap.String("team", team.ID.String()), zap.String("by", creator.ID.String()))
	return out, nil
}

func (w *worker) teams(ctx context.Context, memberships []*models.TeamMember) ([]*pb.Team, error) {
	roles := make(map[gocql.UUID]models.TeamRole, len(memberships))
	teamIDs := make([]gocql.UUID, 0, len(memberships))
	for _, m := range memberships {
		roles[m.TeamID] = m.Role
		teamIDs = append(teamIDs, m.TeamID)
	}
	out := make([]*pb.Team, 0, len(teamIDs))
	for start := 0; start < len(teamIDs); start += 100 {
		found, err := w.Repo.GetTeams(ctx, teamIDs[start:min(start+100, len(teamIDs))])
		if err != nil {
			return nil, err
		}
		for _, t := range found {
			out = append(out, convert.Team(t, roles[t.ID]))
		}
	}
	slices.SortFunc(out, func(a, b *pb.Team) int {
		return cmp.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
	})
	return out, nil
}

func (w *worker) listTeams(ctx context.Context, p *models.Principal) ([]*pb.Team, error) {
	memberships, err := w.Repo.ListUserTeams(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return w.teams(ctx, memberships)
}

func (w *worker) getTeam(ctx context.Context, msg GetTeam) (*pb.Team, error) {
	var (
		teamID gocql.UUID
		err    error
	)
	if msg.Name != "" {
		name, verr := validate.Slug(msg.Name)
		if verr != nil {
			return nil, errNoTeam
		}
		if teamID, err = w.Repo.GetTeamIDByName(ctx, name); err != nil {
			if ierrors.Is(err, ierrors.CodeNotFound) {
				return nil, errNoTeam
			}
			return nil, err
		}
	} else if teamID, err = parseID(msg.ID, "team"); err != nil {
		return nil, err
	}
	m, err := w.membership(ctx, teamID, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	team, err := w.Repo.GetTeam(ctx, teamID)
	if err != nil {
		return nil, err
	}
	return convert.Team(team, m.Role), nil
}

func (w *worker) listTeamMembers(ctx context.Context, msg ListTeamMembers) (*pb.ListTeamMembersResponse, error) {
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return nil, err
	}
	if _, err := w.membership(ctx, teamID, msg.P.UserID); err != nil {
		return nil, err
	}
	entries, err := w.Repo.ListTeamMembers(ctx, teamID)
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(msg.Query, "@")))
	limit := msg.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	slices.SortFunc(entries, func(a, b *scylla.TeamMemberEntry) int { return cmp.Compare(a.Username, b.Username) })
	// Usernames are unique, so the last one listed continues the listing.
	start, found := slices.BinarySearchFunc(entries, msg.After, func(e *scylla.TeamMemberEntry, after string) int {
		return cmp.Compare(e.Username, after)
	})
	if found {
		start++
	}
	res := &pb.ListTeamMembersResponse{}
	for _, e := range entries[start:] {
		if query != "" && !strings.HasPrefix(e.Username, query) && !strings.HasPrefix(strings.ToLower(e.DisplayName), query) {
			continue
		}
		if len(res.Members) == limit {
			res.Next = res.Members[limit-1].User.Username
			break
		}
		res.Members = append(res.Members, &pb.TeamMember{
			User:     &pb.User{Id: e.UserID.String(), Username: e.Username, DisplayName: e.DisplayName},
			Role:     convert.TeamRole(e.Role),
			JoinedAt: convert.Time(e.JoinedAt),
		})
	}
	userIDs := make([]gocql.UUID, 0, len(res.Members))
	for _, m := range res.Members {
		if id, ok := ids.Parse(m.User.Id); ok {
			userIDs = append(userIDs, id)
		}
	}
	if found, err := w.Users.GetMany(ctx, userIDs); err == nil {
		for _, m := range res.Members {
			if id, ok := ids.Parse(m.User.Id); ok && found[id] != nil {
				m.User.Photo = found[id].Photo
			}
		}
	}
	return res, nil
}

// manage checks the caller may act on another member of teamID.
func (w *worker) manage(ctx context.Context, p *models.Principal, teamID gocql.UUID, act authz.Action) error {
	m, err := w.membership(ctx, teamID, p.UserID)
	if err != nil && p.Role != models.SystemRoleAdmin {
		return err
	}
	var role string
	if m != nil {
		role = string(m.Role)
	}
	if !w.Authz.Can(authz.ObjTeamMember, act, role, string(p.Role)) {
		return ierrors.PermissionDenied()
	}
	return nil
}

// lockTeam holds the team's admin changes until the returned func is
// called.
func (w *worker) lockTeam(ctx context.Context, teamID gocql.UUID) (func(), error) {
	unlock, err := w.teamLocks.Lock(ctx, teamID.String())
	if err != nil {
		return nil, ierrors.Unavailable("the team is busy, try again")
	}
	return unlock, nil
}

// keepAdmin fails when target is the team's only admin.
func (w *worker) keepAdmin(ctx context.Context, teamID, target gocql.UUID) error {
	entries, err := w.Repo.ListTeamMembers(ctx, teamID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Role == models.TeamRoleAdmin && e.UserID != target {
			return nil
		}
	}
	return ierrors.FailedPrecondition("a team needs at least one admin")
}

func (w *worker) removeTeamMember(ctx context.Context, msg RemoveTeamMember) (Done, error) {
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return Done{}, err
	}
	target, err := parseID(msg.UserID, "user")
	if err != nil {
		return Done{}, err
	}
	if target == msg.P.UserID {
		return Done{}, ierrors.FailedPrecondition("you cannot remove yourself")
	}
	if err := w.dropTeamMember(ctx, msg.P, teamID, target); err != nil {
		return Done{}, err
	}
	// The channels go after the team: a join racing the removal then finds
	// no team, or joined in time to be listed here.
	if err := w.leaveChannels(ctx, msg.P, teamID, target); err != nil {
		return Done{}, err
	}
	if err := w.dropBots(ctx, msg.P, teamID, target); err != nil {
		return Done{}, err
	}
	go w.recheckWatches(context.WithoutCancel(ctx), target)
	return Done{}, nil
}

// dropTeamMember removes target from the team, keeping it an admin. A
// removal that stopped short of the channels may be finished.
func (w *worker) dropTeamMember(ctx context.Context, p *models.Principal, teamID, target gocql.UUID) error {
	unlock, err := w.lockTeam(ctx, teamID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := w.manage(ctx, p, teamID, authz.ActRemove); err != nil {
		return err
	}
	m, err := w.membership(ctx, teamID, target)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		in, err := w.Repo.ListUserChannels(ctx, target, teamID)
		if err != nil {
			return err
		}
		if len(in) == 0 {
			return ierrors.NotFound("not a member")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := w.mayChangeAdmin(ctx, p, teamID, m); err != nil {
		return err
	}
	return w.Repo.RemoveTeamMember(ctx, teamID, target)
}

func (w *worker) updateTeamMemberRole(ctx context.Context, msg UpdateTeamMemberRole) (Done, error) {
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return Done{}, err
	}
	target, err := parseID(msg.UserID, "user")
	if err != nil {
		return Done{}, err
	}
	unlock, err := w.lockTeam(ctx, teamID)
	if err != nil {
		return Done{}, err
	}
	defer unlock()
	if err := w.manage(ctx, msg.P, teamID, authz.ActUpdateRole); err != nil {
		return Done{}, err
	}
	current, err := w.membership(ctx, teamID, target)
	if err != nil {
		return Done{}, ierrors.NotFound("not a member")
	}
	if current.Role == msg.Role {
		return Done{}, nil
	}
	if err := w.mayChangeAdmin(ctx, msg.P, teamID, current); err != nil {
		return Done{}, err
	}
	return Done{}, w.Repo.UpdateTeamMemberRole(ctx, teamID, target, msg.Role)
}

// mayChangeAdmin checks, under the team's lock, that p may demote or remove
// target: an admin only the team's owner or a system admin may, the owner
// only themself or a system admin, and never the last admin.
func (w *worker) mayChangeAdmin(ctx context.Context, p *models.Principal, teamID gocql.UUID, target *models.TeamMember) error {
	if target.Role != models.TeamRoleAdmin {
		return nil
	}
	team, err := w.Repo.GetTeam(ctx, teamID)
	if err != nil {
		return err
	}
	if p.Role != models.SystemRoleAdmin {
		owner := p.UserID == team.CreatedBy
		switch {
		case target.UserID == team.CreatedBy && !owner:
			return ierrors.New(ierrors.CodePermissionDenied, "only the team's owner or a system admin can change the owner's role")
		case !owner && target.UserID != p.UserID:
			return ierrors.New(ierrors.CodePermissionDenied, "only the team's owner can demote or remove another admin")
		}
	}
	return w.keepAdmin(ctx, teamID, target.UserID)
}

// inviter returns the team of rawID when p may manage its invites.
func (w *worker) inviter(ctx context.Context, p *models.Principal, rawID string) (gocql.UUID, error) {
	teamID, err := parseID(rawID, "team")
	if err != nil {
		return gocql.UUID{}, err
	}
	m, err := w.membership(ctx, teamID, p.UserID)
	if err != nil {
		return gocql.UUID{}, err
	}
	if !w.Authz.Can(authz.ObjInvite, authz.ActCreate, string(m.Role), string(p.Role)) {
		return gocql.UUID{}, ierrors.PermissionDenied()
	}
	return teamID, nil
}

func (w *worker) createInvite(ctx context.Context, msg CreateInvite) (*Invite, error) {
	teamID, err := w.inviter(ctx, msg.P, msg.TeamID)
	if err != nil {
		return nil, err
	}
	if key := msg.P.UserID.String(); w.invited.Blocked(key) {
		return nil, ierrors.ResourceExhausted("too many invites created, try again later")
	} else {
		w.invited.Add(key)
	}
	token := secure.Token()
	now := time.Now().UTC()
	inv := &models.Invite{
		TokenHash: secure.Hash(token),
		TeamID:    teamID,
		CreatedBy: msg.P.UserID,
		CreatedAt: now,
		ExpiresAt: now.Add(w.Cfg.InviteTTL),
	}
	if err := w.Repo.CreateInvite(ctx, inv); err != nil {
		return nil, err
	}
	return &Invite{Token: token, ExpiresAt: inv.ExpiresAt}, nil
}

// invitePage is how many invites a listing shows at once.
const invitePage = 50

// inviteKey orders invites newest first, and continues their listing.
type inviteKey struct {
	at int64
	id string
}

func keyOf(inv *models.Invite) inviteKey {
	return inviteKey{at: inv.CreatedAt.UnixMicro(), id: secure.PublicID(inv.TokenHash)}
}

func (k inviteKey) compare(o inviteKey) int {
	if c := cmp.Compare(o.at, k.at); c != 0 {
		return c
	}
	return cmp.Compare(k.id, o.id)
}

func (k inviteKey) String() string { return strconv.FormatInt(k.at, 10) + "." + k.id }

var inviteCursorRe = regexp.MustCompile(`^(\d{1,19})\.([0-9a-f]{24})$`)

func parseInviteKey(s string) (inviteKey, bool) {
	m := inviteCursorRe.FindStringSubmatch(s)
	if m == nil {
		return inviteKey{}, false
	}
	at, err := strconv.ParseInt(m[1], 10, 64)
	return inviteKey{at: at, id: m[2]}, err == nil
}

func (w *worker) listInvites(ctx context.Context, msg ListInvites) (*pb.ListInvitesResponse, error) {
	teamID, err := w.inviter(ctx, msg.P, msg.TeamID)
	if err != nil {
		return nil, err
	}
	var after inviteKey
	if msg.After != "" {
		var ok bool
		if after, ok = parseInviteKey(msg.After); !ok {
			return nil, ierrors.InvalidArgument("invalid invite cursor")
		}
	}
	listed, err := w.Repo.ListTeamInvites(ctx, teamID)
	if err != nil {
		return nil, err
	}
	keys := make(map[*models.Invite]inviteKey, len(listed))
	for _, inv := range listed {
		keys[inv] = keyOf(inv)
	}
	slices.SortFunc(listed, func(a, b *models.Invite) int { return keys[a].compare(keys[b]) })
	if msg.After != "" {
		start, _ := slices.BinarySearchFunc(listed, after, func(inv *models.Invite, k inviteKey) int {
			if c := keys[inv].compare(k); c != 0 {
				return c
			}
			return -1
		})
		listed = listed[start:]
	}
	// One more than a page tells whether another follows.
	var page []*models.Invite
	for len(page) <= invitePage && len(listed) > 0 {
		n := min(len(listed), invitePage+1-len(page))
		open, err := w.Repo.OpenInvites(ctx, teamID, listed[:n])
		if err != nil {
			return nil, err
		}
		page = append(page, open...)
		listed = listed[n:]
	}
	res := &pb.ListInvitesResponse{}
	if len(page) > invitePage {
		page = page[:invitePage]
		res.Next = keys[page[invitePage-1]].String()
	}
	creatorIDs := make([]gocql.UUID, 0, len(page))
	for _, inv := range page {
		if !slices.Contains(creatorIDs, inv.CreatedBy) {
			creatorIDs = append(creatorIDs, inv.CreatedBy)
		}
	}
	creators, err := w.Users.GetMany(ctx, creatorIDs)
	if err != nil {
		return nil, err
	}
	active := make(map[gocql.UUID]bool, len(creatorIDs))
	for _, id := range creatorIDs {
		ok, err := invites.CanInvite(ctx, w.Repo, w.Authz, w.Users, teamID, id)
		if err != nil && !ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, err
		}
		active[id] = ok
	}
	for _, inv := range page {
		s := &pb.InviteSummary{
			Id: keys[inv].id, CreatedAt: convert.Time(inv.CreatedAt), ExpiresAt: convert.Time(inv.ExpiresAt),
			Active: active[inv.CreatedBy],
		}
		if u := creators[inv.CreatedBy]; u != nil {
			s.CreatedBy = convert.PublicUser(u)
		}
		res.Invites = append(res.Invites, s)
	}
	return res, nil
}

var inviteIDRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

func (w *worker) revokeInvite(ctx context.Context, msg RevokeInvite) (Done, error) {
	teamID, err := w.inviter(ctx, msg.P, msg.TeamID)
	if err != nil {
		return Done{}, err
	}
	if !inviteIDRe.MatchString(msg.InviteID) {
		return Done{}, ierrors.InvalidArgument("invalid invite id")
	}
	// The id is the start of the token hash, which orders the team's list.
	prefix, _ := hex.DecodeString(msg.InviteID)
	invs, err := w.Repo.FindTeamInvites(ctx, teamID, prefix)
	if err != nil {
		return Done{}, err
	}
	for _, inv := range invs {
		if secure.PublicID(inv.TokenHash) != msg.InviteID {
			continue
		}
		revoked, err := w.Repo.RevokeInvite(ctx, teamID, inv.TokenHash)
		if err != nil {
			return Done{}, err
		}
		if !revoked {
			return Done{}, ierrors.FailedPrecondition("the invite was used or has expired")
		}
		w.Logger.Info("invite revoked", zap.String("team", teamID.String()), zap.String("invite", msg.InviteID), zap.String("by", msg.P.UserID.String()))
		return Done{}, nil
	}
	return Done{}, ierrors.NotFound("invite not found")
}

func (w *worker) getInvite(ctx context.Context, token string) (*InviteInfo, error) {
	inv, err := invites.Valid(ctx, w.Repo, w.Authz, w.Users, token, gocql.UUID{})
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return &InviteInfo{}, nil
		}
		return nil, err
	}
	team, err := w.Repo.GetTeam(ctx, inv.TeamID)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return &InviteInfo{}, nil
		}
		return nil, err
	}
	return &InviteInfo{Valid: true, TeamDisplayName: team.DisplayName}, nil
}

func (w *worker) acceptInvite(ctx context.Context, msg AcceptInvite) (*pb.Team, error) {
	inv, err := invites.Valid(ctx, w.Repo, w.Authz, w.Users, msg.Token, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	if _, err := w.Repo.GetTeamMember(ctx, inv.TeamID, msg.P.UserID); err == nil {
		// Already a member: the invite stays unused for someone else.
		return w.joinTeam(ctx, msg.P.UserID, inv.TeamID)
	}
	// An invite the caller used in a join that did not complete is theirs
	// to finish.
	consumed := false
	if inv.UsedBy != msg.P.UserID {
		ok, err := w.Repo.ConsumeInvite(ctx, inv, msg.P.UserID, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, invites.ErrInvalid
		}
		consumed = true
	}
	team, err := w.joinTeam(ctx, msg.P.UserID, inv.TeamID)
	if err != nil {
		if consumed {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = w.Repo.ReleaseInvite(c, inv, msg.P.UserID, time.Now())
		}
		return nil, err
	}
	w.spendInvite(ctx, inv.TokenHash, msg.P.UserID)
	return team, nil
}

// spendInvite deletes an invite whose join completed. Until then its user
// may finish the join with it; after, it must not readmit them once removed.
func (w *worker) spendInvite(ctx context.Context, hash []byte, userID gocql.UUID) {
	if err := w.Repo.SpendInvite(ctx, hash, userID); err != nil {
		w.Logger.Warn("failed to spend a used invite", zap.Error(err))
	}
}

// joinTeam makes userID a member of teamID and of its default channels.
func (w *worker) joinTeam(ctx context.Context, userID, teamID gocql.UUID) (*pb.Team, error) {
	team, err := w.Repo.GetTeam(ctx, teamID)
	if err != nil {
		return nil, err
	}
	user, err := w.Users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	role := models.TeamRoleMember
	m, err := w.Repo.GetTeamMember(ctx, teamID, userID)
	switch {
	case err == nil:
		// A member in no channel of the team has a join to finish.
		in, err := w.Repo.ListUserChannels(ctx, userID, teamID)
		if err != nil || len(in) > 0 {
			return convert.Team(team, m.Role), err
		}
		role = m.Role
	case ierrors.Is(err, ierrors.CodeNotFound):
		member := &models.TeamMember{TeamID: teamID, UserID: userID, Role: role, JoinedAt: time.Now().UTC()}
		if err := w.Repo.AddTeamMember(ctx, member, user.Username, user.DisplayName); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	p := &models.Principal{UserID: userID, Role: user.Role}
	for _, dc := range defaultChannels {
		chID, err := w.Repo.GetChannelIDByName(ctx, teamID, dc.name)
		if err != nil {
			if ierrors.Is(err, ierrors.CodeNotFound) {
				continue
			}
			return nil, err
		}
		join := channels.Join{Target: channels.Target{P: p, ChannelID: chID}, System: true}
		if _, err := actors.Ask[*pb.Channel](ctx, w.Engine, w.Channels, join); err != nil {
			return nil, err
		}
	}
	out := convert.Team(team, role)
	w.Engine.Send(w.Voice, voice.TeamChanged{TeamID: teamID})
	w.deliver(&pb.Event{Payload: &pb.Event_TeamJoined{TeamJoined: &pb.TeamJoined{Team: out}}}, userID)
	return out, nil
}

func (w *worker) listChannels(ctx context.Context, msg ListChannels) ([]*pb.Channel, error) {
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return nil, err
	}
	if _, err := w.membership(ctx, teamID, msg.P.UserID); err != nil {
		return nil, err
	}
	// A team's conversations are its own, filed with its channels.
	memberships, err := w.Repo.ListUserChannels(ctx, msg.P.UserID, teamID)
	if err != nil {
		return nil, err
	}
	roles := make(map[gocql.UUID]models.ChannelRole, len(memberships))
	chIDs := make([]gocql.UUID, 0, len(memberships))
	for _, m := range memberships {
		roles[m.ChannelID] = m.Role
		chIDs = append(chIDs, m.ChannelID)
	}
	// Every member of a team sees its voice channels, as rooms to talk in.
	listed, err := w.Repo.ListTeamChannels(ctx, teamID)
	if err != nil {
		return nil, err
	}
	for _, tc := range listed {
		if _, mine := roles[tc.ChannelID]; tc.Kind == models.ChannelVoice && !mine {
			chIDs = append(chIDs, tc.ChannelID)
		}
	}
	reads, err := w.Repo.ListLastReads(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	lastRead := make(map[gocql.UUID]gocql.UUID, len(reads))
	for _, r := range reads {
		lastRead[r.ChannelID] = r.LastReadID
	}
	mentions, err := w.Repo.CountMentions(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	muted, err := w.Repo.ListMuted(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	cleared, err := w.Repo.ListCleared(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	pts, err := w.Repo.ChannelsPts(ctx, chIDs)
	if err != nil {
		return nil, err
	}
	var all []*models.Channel
	for start := 0; start < len(chIDs); start += 100 {
		found, err := w.Repo.GetChannels(ctx, chIDs[start:min(start+100, len(chIDs))])
		if err != nil {
			return nil, err
		}
		all = append(all, found...)
	}
	var peerIDs []gocql.UUID
	for _, ch := range all {
		if ch.Kind.Conversation() {
			peerIDs = append(peerIDs, convert.PeerIDs(ch, msg.P.UserID)...)
		}
	}
	peers, err := w.Users.GetMany(ctx, peerIDs)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Channel, 0, len(all))
	for _, ch := range all {
		// A cleared conversation shows again with its next message.
		if c, ok := cleared[ch.ID]; !ch.ArchivedAt.IsZero() || (ok && !ids.After(ch.LastMessageID, c)) {
			continue
		}
		view := convert.Conversation(ch, roles[ch.ID], lastRead[ch.ID], msg.P.UserID, peers)
		if ch.Kind.Conversation() && view.Peer == nil && len(view.Peers) == 0 {
			continue
		}
		view.MentionCount = int32(min(mentions[ch.ID], 9999)) //nolint:gosec // bounded
		view.Muted = muted[ch.ID]
		view.Pts = pts[ch.ID]
		out = append(out, view)
	}
	slices.SortFunc(out, func(a, b *pb.Channel) int { return cmp.Compare(sortKey(a), sortKey(b)) })
	return out, nil
}

func sortKey(ch *pb.Channel) string {
	if ch.Peer != nil {
		return strings.ToLower(ch.Peer.DisplayName)
	}
	if len(ch.Peers) > 0 {
		names := make([]string, len(ch.Peers))
		for i, p := range ch.Peers {
			names[i] = p.DisplayName
		}
		return strings.ToLower(strings.Join(names, ", "))
	}
	return strings.ToLower(ch.DisplayName)
}

func (w *worker) browseChannels(ctx context.Context, msg BrowseChannels) (*Browsed, error) {
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return nil, err
	}
	m, err := w.membership(ctx, teamID, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	if !w.Authz.Can(authz.ObjChannel, authz.ActBrowse, string(m.Role), string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	listed, err := w.Repo.ListTeamChannels(ctx, teamID)
	if err != nil {
		return nil, err
	}
	mine, err := w.Repo.ListUserChannels(ctx, msg.P.UserID, teamID)
	if err != nil {
		return nil, err
	}
	joined := make(map[gocql.UUID]bool, len(mine))
	for _, cm := range mine {
		joined[cm.ChannelID] = true
	}
	var visible []gocql.UUID
	for _, tc := range listed {
		if tc.Kind == models.ChannelPublic || joined[tc.ChannelID] {
			visible = append(visible, tc.ChannelID)
		}
	}
	out := &Browsed{}
	for start := 0; start < len(visible); start += 100 {
		found, err := w.Repo.GetChannels(ctx, visible[start:min(start+100, len(visible))])
		if err != nil {
			return nil, err
		}
		for _, ch := range found {
			switch {
			case !ch.ArchivedAt.IsZero():
				if joined[ch.ID] {
					out.Archived = append(out.Archived, convert.Channel(ch, "", gocql.UUID{}, nil))
				}
			case !joined[ch.ID]:
				out.Open = append(out.Open, convert.Channel(ch, "", gocql.UUID{}, nil))
			}
		}
	}
	byName := func(a, b *pb.Channel) int { return cmp.Compare(sortKey(a), sortKey(b)) }
	slices.SortFunc(out.Open, byName)
	slices.SortFunc(out.Archived, byName)
	return out, nil
}

func (w *worker) createChannel(ctx context.Context, msg CreateChannel) (*pb.Channel, error) {
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return nil, err
	}
	m, err := w.membership(ctx, teamID, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	act := authz.ActCreatePublic
	switch msg.Kind {
	case models.ChannelPublic:
	case models.ChannelPrivate:
		act = authz.ActCreatePrivate
	case models.ChannelVoice:
		act = authz.ActCreateVoice
	default:
		return nil, ierrors.InvalidArgument("channel type must be public, private or voice")
	}
	if !w.Authz.Can(authz.ObjChannel, act, string(m.Role), string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	name, err := validate.Slug(msg.Name)
	if err != nil {
		return nil, err
	}
	display, err := validate.Line("display name", msg.DisplayName, 1, 64)
	if err != nil {
		return nil, err
	}
	purpose, err := validate.Line("purpose", msg.Purpose, 0, 250)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ch := &models.Channel{
		ID: ids.New(), TeamID: teamID, Name: name, DisplayName: display, Purpose: purpose,
		Kind: msg.Kind, CreatedBy: msg.P.UserID, CreatedAt: now, VoiceDisabled: msg.VoiceDisabled,
	}
	cm := &models.ChannelMember{ChannelID: ch.ID, UserID: msg.P.UserID, TeamID: teamID, Role: models.ChannelRoleAdmin, JoinedAt: now}
	if err := w.Repo.CreateChannel(ctx, ch, cm); err != nil {
		return nil, err
	}
	out := convert.Channel(ch, models.ChannelRoleAdmin, gocql.UUID{}, nil)
	w.deliver(&pb.Event{Payload: &pb.Event_ChannelJoined{ChannelJoined: &pb.ChannelJoined{Channel: out}}}, msg.P.UserID)
	if ch.Kind == models.ChannelVoice {
		w.announce(ctx, ch, msg.P.UserID)
	}
	return out, nil
}

// announce shows the rest of the team a new voice channel, which is
// theirs to talk in.
func (w *worker) announce(ctx context.Context, ch *models.Channel, creator gocql.UUID) {
	members, err := w.Repo.ListTeamMembers(ctx, ch.TeamID)
	if err != nil {
		w.Logger.Warn("failed to announce a voice channel", zap.String("channel", ch.ID.String()), zap.Error(err))
		return
	}
	to := make([]gocql.UUID, 0, len(members))
	for _, m := range members {
		if m.UserID != creator {
			to = append(to, m.UserID)
		}
	}
	view := convert.Channel(ch, "", gocql.UUID{}, nil)
	w.deliver(&pb.Event{Payload: &pb.Event_ChannelJoined{ChannelJoined: &pb.ChannelJoined{Channel: view}}}, to...)
}

func (w *worker) getUser(ctx context.Context, msg GetUser) (*pb.User, error) {
	id, err := parseID(msg.UserID, "user")
	if err != nil {
		return nil, err
	}
	u, err := w.Users.Get(ctx, id)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, ierrors.NotFound("user not found")
		}
		return nil, err
	}
	if id == msg.P.UserID {
		return convert.User(u), nil
	}
	return convert.PublicUser(u), nil
}

func (w *worker) updateProfile(ctx context.Context, msg UpdateProfile) (*pb.User, error) {
	name, err := validate.Line("display name", msg.DisplayName, 1, 64)
	if err != nil {
		return nil, err
	}
	if err := w.Repo.UpdateDisplayName(ctx, msg.P.UserID, name, time.Now().UTC()); err != nil {
		return nil, err
	}
	if err := w.Repo.UpdateMemberDisplayName(ctx, msg.P.UserID, name); err != nil {
		w.Logger.Warn("failed to update member display names", zap.Error(err))
	}
	w.Users.Invalidate(msg.P.UserID)
	u, err := w.Repo.GetUser(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	return convert.User(u), nil
}

func (w *worker) me(ctx context.Context, p *models.Principal) (*MeResult, error) {
	u, err := w.Repo.GetUser(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return &MeResult{User: convert.User(u), Email: u.Email}, nil
}

func (w *worker) listMuted(ctx context.Context, p *models.Principal) ([]string, error) {
	muted, err := w.Repo.ListMuted(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(muted))
	for id := range muted {
		out = append(out, id.String())
	}
	return out, nil
}
