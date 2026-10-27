// Package voice keeps who talks in which voice channel, as Discord's gateway
// keeps voice states. People join through the chat server, which hands them
// a voice server and a single-use token; the voice server claims the token
// when they connect to it and reports when they leave. Each person talks in
// one voice channel at a time, and all of a channel's talk runs on one
// voice server.
package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/config"
	"github.com/ssenerg/gochat/internal/core/cache"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/hub"
)

type Config struct {
	// TokenTTL is how long a joiner has to reach the voice server.
	TokenTTL time.Duration `mapstructure:"token_ttl"`
	// MaxPerChannel bounds how many people talk in one voice channel.
	MaxPerChannel int `mapstructure:"max_per_channel"`
	// MaxPendingPerUser bounds the tokens one person holds unclaimed.
	MaxPendingPerUser int `mapstructure:"max_pending_per_user"`
	// ServerQueueLimit is how many messages a voice server may lag behind
	// before it is dropped.
	ServerQueueLimit int64 `mapstructure:"server_queue_limit"`
	// MembersTTL is how long the members of a team, whom its voice changes
	// reach, are remembered.
	MembersTTL time.Duration `mapstructure:"members_ttl"`
	OpTimeout  time.Duration `mapstructure:"op_timeout"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.TokenTTL < time.Second || c.TokenTTL > 5*time.Minute {
		return errors.New("token_ttl must be between 1s and 5m")
	}
	if c.MaxPerChannel < 2 || c.MaxPerChannel > 500 {
		return errors.New("max_per_channel must be between 2 and 500")
	}
	if c.MaxPendingPerUser < 1 {
		return errors.New("max_pending_per_user must be at least 1")
	}
	if c.ServerQueueLimit < 16 {
		return errors.New("server_queue_limit must be at least 16")
	}
	if c.MembersTTL < 0 || c.OpTimeout <= 0 {
		return errors.New("members_ttl cannot be negative and op_timeout must be positive")
	}
	return nil
}

type Repo interface {
	GetTeamMember(ctx context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error)
	ListTeamMembers(ctx context.Context, teamID gocql.UUID) ([]*scylla.TeamMemberEntry, error)
}

type Deps struct {
	Logger *zap.Logger
	Cfg    *Config
	Repo   Repo
	Engine *actor.Engine
	Hub    *actor.PID
}

// Stream is the sending half of a voice server's Connect stream.
type Stream interface {
	Send(*pb.VoiceServerServiceConnectResponse) error
	Close() error
}

// Messages of the people who talk, which the API checked: Join's caller may
// connect to Channel, and Kick's may moderate ChannelID.
type (
	// Join replies with a *Ticket.
	Join struct {
		P       *models.Principal
		User    *pb.User
		Channel *models.Channel
	}
	Ticket struct{ Endpoint, Token string }
	Leave  struct{ UserID gocql.UUID }
	Update struct {
		UserID     gocql.UUID
		Mute, Deaf bool
	}
	Kick struct{ ChannelID, UserID gocql.UUID }
	// List replies with the []*pb.VoiceChannelState of TeamID when the
	// caller belongs to the team.
	List struct {
		P      *models.Principal
		TeamID gocql.UUID
	}
	Done struct{}
)

// Messages about the people who may no longer talk.
type (
	// DropUser disconnects a user removed from TeamID.
	DropUser struct{ UserID, TeamID gocql.UUID }
	// TeamChanged says someone joined the team, who should hear of its
	// voice channels from now on.
	TeamChanged struct{ TeamID gocql.UUID }
	// RevokeSessions disconnects whoever joined from a revoked session, or
	// with All from any of the user's.
	RevokeSessions struct {
		UserID     gocql.UUID
		SessionIDs []string
		All        bool
	}
)

// Messages of the voice servers' streams; Key names one stream.
type (
	// ServerUp replies with Registered.
	ServerUp struct {
		Key, ID, Endpoint string
		Stream            Stream
	}
	Registered struct{}
	ServerDown struct{ Key string }
	Claim      struct {
		Key       string
		RequestID uint64
		Token     string
	}
	PeerLeft struct {
		Key                        string
		UserID, ChannelID, Session string
	}
	// PeerMedia says whether a voice server forwards a peer's camera, and
	// its screen.
	PeerMedia struct {
		Key                        string
		UserID, ChannelID, Session string
		Video, Stream              bool
	}
)

type sweep struct{}

type pending struct {
	user      *pb.User
	userID    gocql.UUID
	channelID gocql.UUID
	teamID    gocql.UUID
	gwSession string
	// gwExpires is when the sign-in the person joined with ends, and
	// their talk with it.
	gwExpires time.Time
	server    string
	expires   time.Time
}

type state struct {
	user      *pb.User
	userID    gocql.UUID
	channelID gocql.UUID
	teamID    gocql.UUID
	// session names this voice connection, gwSession the sign-in it came
	// from.
	session    string
	gwSession  string
	gwExpires  time.Time
	server     string
	mute, deaf bool
	// video and stream say they send their camera, and their screen.
	video, stream bool
}

type room struct {
	teamID  gocql.UUID
	server  string
	users   []gocql.UUID
	pending int
	version uint64
}

type server struct {
	key      string
	endpoint string
	stream   Stream
	pid      *actor.PID
	queued   *atomic.Int64
	peers    int
}

type voice struct {
	*Deps
	servers map[string]*server
	pending map[string]*pending
	// held counts each person's unclaimed tokens.
	held   map[gocql.UUID]int
	states map[gocql.UUID]*state
	rooms  map[gocql.UUID]*room
	// versions outlive the rooms: a channel's talk that ends and starts
	// again keeps counting up, so pages never take a new state for an old
	// one.
	versions map[gocql.UUID]uint64
	members  *cache.Cache[gocql.UUID, []string]
	sweeper  *actor.SendRepeater
}

func New(d *Deps) actor.Producer {
	return func() actor.Receiver {
		return &voice{
			Deps:     d,
			servers:  make(map[string]*server),
			pending:  make(map[string]*pending),
			held:     make(map[gocql.UUID]int),
			states:   make(map[gocql.UUID]*state),
			rooms:    make(map[gocql.UUID]*room),
			versions: make(map[gocql.UUID]uint64),
			members:  cache.New[gocql.UUID, []string](10_000, d.Cfg.MembersTTL),
		}
	}
}

func (v *voice) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		r := c.SendRepeat(c.PID(), sweep{}, v.Cfg.TokenTTL)
		v.sweeper = &r
	case actor.Stopped:
		if v.sweeper != nil {
			v.sweeper.Stop()
		}
		for _, s := range v.servers {
			closeStream(s.stream)
		}
	case sweep:
		v.sweep()
	case Join:
		out, err := v.join(msg)
		v.reply(c, out, err)
	case Leave:
		if st := v.states[msg.UserID]; st != nil {
			v.remove(st, "left")
		}
		actors.Reply(c, Done{})
	case Update:
		out, err := v.update(msg)
		v.reply(c, out, err)
	case Kick:
		out, err := v.kick(msg)
		v.reply(c, out, err)
	case List:
		v.list(c, msg)
	case TeamChanged:
		v.members.Delete(msg.TeamID)
	case DropUser:
		v.members.Delete(msg.TeamID)
		if st := v.states[msg.UserID]; st != nil && (msg.TeamID == (gocql.UUID{}) || st.teamID == msg.TeamID) {
			v.remove(st, "removed from the team")
		}
	case RevokeSessions:
		if st := v.states[msg.UserID]; st != nil && (msg.All || slices.Contains(msg.SessionIDs, st.gwSession)) {
			v.remove(st, "signed out")
		}
	case ServerUp:
		v.serverUp(c, msg)
	case ServerDown:
		v.serverDown(c, msg.Key)
	case Claim:
		v.claim(c, msg)
	case PeerLeft:
		v.peerLeft(msg)
	case PeerMedia:
		v.peerMedia(msg)
	}
}

// reply answers an Ask with out or err.
func (v *voice) reply(c *actor.Context, out any, err error) {
	if err != nil {
		actors.Reply(c, err)
		return
	}
	actors.Reply(c, out)
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (v *voice) join(msg Join) (*Ticket, error) {
	chID := msg.Channel.ID
	rm := v.rooms[chID]
	inside := 0
	if st := v.states[msg.P.UserID]; st != nil && st.channelID == chID {
		inside = 1
	}
	if rm != nil && len(rm.users)-inside >= v.Cfg.MaxPerChannel {
		return nil, ierrors.ResourceExhausted("this voice channel is full")
	}
	if v.held[msg.P.UserID] >= v.Cfg.MaxPendingPerUser {
		return nil, ierrors.ResourceExhausted("you are joining too fast: wait a moment")
	}
	if rm == nil {
		srv := v.leastLoaded()
		if srv == nil {
			return nil, ierrors.Unavailable("voice is unavailable right now: no voice server is connected")
		}
		rm = &room{teamID: msg.Channel.TeamID, server: srv.key}
		v.rooms[chID] = rm
	}
	token := secure.Token()
	v.pending[string(secure.Hash(token))] = &pending{
		user: msg.User, userID: msg.P.UserID, channelID: chID, teamID: msg.Channel.TeamID,
		gwSession: msg.P.SessionID, gwExpires: msg.P.ExpiresAt, server: rm.server, expires: time.Now().Add(v.Cfg.TokenTTL),
	}
	v.held[msg.P.UserID]++
	rm.pending++
	return &Ticket{Endpoint: v.servers[rm.server].endpoint, Token: token}, nil
}

func (v *voice) leastLoaded() *server {
	var best *server
	for _, s := range v.servers {
		if best == nil || s.peers < best.peers {
			best = s
		}
	}
	return best
}

func (v *voice) update(msg Update) (Done, error) {
	st := v.states[msg.UserID]
	if st == nil {
		return Done{}, ierrors.FailedPrecondition("you are not in a voice channel")
	}
	// Deafened people cannot be heard either, as in Discord.
	mute := msg.Mute || msg.Deaf
	if st.mute != mute || st.deaf != msg.Deaf {
		st.mute, st.deaf = mute, msg.Deaf
		v.changed(st.channelID)
	}
	return Done{}, nil
}

func (v *voice) kick(msg Kick) (Done, error) {
	st := v.states[msg.UserID]
	if st == nil || st.channelID != msg.ChannelID {
		return Done{}, ierrors.NotFound("they are not in this voice channel")
	}
	v.remove(st, "disconnected by a moderator")
	return Done{}, nil
}

// list answers off the actor: membership is read from the database.
func (v *voice) list(c *actor.Context, msg List) {
	var out []*pb.VoiceChannelState
	for id, rm := range v.rooms {
		if rm.teamID == msg.TeamID && len(rm.users) > 0 {
			out = append(out, v.snapshot(id, rm))
		}
	}
	sender, engine := c.Sender(), c.Engine()
	if !actors.IsAsk(sender) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), v.Cfg.OpTimeout)
		defer cancel()
		if msg.P.Role != models.SystemRoleAdmin {
			if _, err := v.Repo.GetTeamMember(ctx, msg.TeamID, msg.P.UserID); err != nil {
				if ierrors.Is(err, ierrors.CodeNotFound) {
					err = ierrors.NotFound("team not found")
				}
				engine.Send(sender, err)
				return
			}
		}
		engine.Send(sender, out)
	}()
}

func (v *voice) snapshot(id gocql.UUID, rm *room) *pb.VoiceChannelState {
	out := &pb.VoiceChannelState{ChannelId: id.String(), Version: rm.version}
	for _, uid := range rm.users {
		st := v.states[uid]
		out.States = append(out.States, &pb.VoiceState{
			User: st.user, ChannelId: out.ChannelId, SelfMute: st.mute, SelfDeaf: st.deaf, SelfVideo: st.video, SelfStream: st.stream,
		})
	}
	return out
}

// changed tells the team of a voice channel who talks in it now.
func (v *voice) changed(channelID gocql.UUID) {
	rm := v.rooms[channelID]
	if rm == nil {
		return
	}
	v.versions[channelID]++
	rm.version = v.versions[channelID]
	ev := &pb.Event{Payload: &pb.Event_VoiceChannelUpdate{VoiceChannelUpdate: &pb.VoiceChannelUpdate{
		TeamId: rm.teamID.String(), Channel: v.snapshot(channelID, rm),
	}}}
	teamID := rm.teamID
	if ids, ok := v.members.Get(teamID); ok {
		v.Engine.Send(v.Hub, hub.Deliver{UserIDs: ids, Event: ev})
		return
	}
	// The versions keep these deliveries, which may pass each other, in
	// order on the pages.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), v.Cfg.OpTimeout)
		defer cancel()
		list, err := v.Repo.ListTeamMembers(ctx, teamID)
		if err != nil {
			v.Logger.Warn("failed to list a team for a voice change", zap.String("team", teamID.String()), zap.Error(err))
			return
		}
		ids := make([]string, len(list))
		for i, m := range list {
			ids[i] = m.UserID.String()
		}
		v.members.Set(teamID, ids)
		v.Engine.Send(v.Hub, hub.Deliver{UserIDs: ids, Event: ev})
	}()
}

// remove ends a person's talk, and tells their voice server to drop them
// unless reason is empty: it reported they left.
func (v *voice) remove(st *state, reason string) {
	delete(v.states, st.userID)
	if s := v.servers[st.server]; s != nil {
		s.peers--
		if reason != "" {
			v.send(s, &pb.VoiceServerServiceConnectResponse{Frame: &pb.VoiceServerServiceConnectResponse_Disconnect{Disconnect: &pb.VoiceDisconnect{
				UserId: st.userID.String(), ChannelId: st.channelID.String(), Session: st.session, Reason: reason,
			}}})
		}
	}
	rm := v.rooms[st.channelID]
	if rm == nil {
		return
	}
	rm.users = slices.DeleteFunc(rm.users, func(id gocql.UUID) bool { return id == st.userID })
	v.changed(st.channelID)
	if len(rm.users) == 0 && rm.pending == 0 {
		delete(v.rooms, st.channelID)
	}
}

func (v *voice) claimed(s *server, out *pb.VoiceTokenClaimed) {
	v.send(s, &pb.VoiceServerServiceConnectResponse{Frame: &pb.VoiceServerServiceConnectResponse_Claimed{Claimed: out}})
}

func (v *voice) claim(c *actor.Context, msg Claim) {
	s := v.servers[msg.Key]
	if s == nil {
		return
	}
	refuse := func(text string) {
		v.claimed(s, &pb.VoiceTokenClaimed{RequestId: msg.RequestID, Error: text})
	}
	if !secure.ValidToken(msg.Token) {
		refuse("unknown or expired token")
		return
	}
	hash := string(secure.Hash(msg.Token))
	p := v.pending[hash]
	if p == nil || p.server != msg.Key {
		refuse("unknown or expired token")
		return
	}
	v.forget(hash, p)
	rm := v.rooms[p.channelID]
	if rm == nil || rm.server != msg.Key || time.Now().After(p.expires) {
		if rm != nil {
			rm.pending--
			v.dropIfIdle(p.channelID)
		}
		refuse("unknown or expired token")
		return
	}
	// The claim's pending count keeps the room while the person's earlier
	// talk, perhaps in this very room, ends.
	if old := v.states[p.userID]; old != nil {
		v.remove(old, "joined from somewhere else")
	}
	rm.pending--
	if len(rm.users) >= v.Cfg.MaxPerChannel {
		v.dropIfIdle(p.channelID)
		refuse("this voice channel is full")
		return
	}
	st := &state{
		user: p.user, userID: p.userID, channelID: p.channelID, teamID: p.teamID,
		session: randomID(), gwSession: p.gwSession, gwExpires: p.gwExpires, server: msg.Key,
	}
	v.states[p.userID] = st
	rm.users = append(rm.users, p.userID)
	s.peers++
	v.changed(p.channelID)
	v.claimed(s, &pb.VoiceTokenClaimed{
		RequestId: msg.RequestID, User: p.user, ChannelId: p.channelID.String(), Session: st.session,
	})
}

func (v *voice) dropIfIdle(channelID gocql.UUID) {
	if rm := v.rooms[channelID]; rm != nil && len(rm.users) == 0 && rm.pending <= 0 {
		delete(v.rooms, channelID)
	}
}

func (v *voice) peerLeft(msg PeerLeft) {
	for _, st := range v.states {
		if st.server == msg.Key && st.session == msg.Session && st.userID.String() == msg.UserID && st.channelID.String() == msg.ChannelID {
			v.remove(st, "")
			return
		}
	}
}

// peerMedia shows a camera and a screen as the voice server forwards
// them: what a client says of itself shows nobody anything.
func (v *voice) peerMedia(msg PeerMedia) {
	for _, st := range v.states {
		if st.server == msg.Key && st.session == msg.Session && st.userID.String() == msg.UserID && st.channelID.String() == msg.ChannelID {
			if st.video != msg.Video || st.stream != msg.Stream {
				st.video, st.stream = msg.Video, msg.Stream
				v.changed(st.channelID)
			}
			return
		}
	}
}

// sweep forgets the tokens nobody claimed in time.
func (v *voice) sweep() {
	now := time.Now()
	for hash, p := range v.pending {
		if now.After(p.expires) {
			v.forget(hash, p)
			if rm := v.rooms[p.channelID]; rm != nil {
				rm.pending--
			}
			v.dropIfIdle(p.channelID)
		}
	}
	for _, st := range v.states {
		if !st.gwExpires.IsZero() && now.After(st.gwExpires) {
			v.remove(st, "your sign-in expired")
		}
	}
}

// forget drops an unclaimed token.
func (v *voice) forget(hash string, p *pending) {
	delete(v.pending, hash)
	if v.held[p.userID]--; v.held[p.userID] <= 0 {
		delete(v.held, p.userID)
	}
}

func (v *voice) serverUp(c *actor.Context, msg ServerUp) {
	s := &server{key: msg.Key, endpoint: msg.Endpoint, stream: msg.Stream, queued: new(atomic.Int64)}
	s.pid = c.SpawnChild(newWriter(v.Logger, msg.Key, msg.Stream, s.queued), "server", actor.WithMiddleware(actors.Recover(v.Logger)))
	v.servers[msg.Key] = s
	v.send(s, &pb.VoiceServerServiceConnectResponse{Frame: &pb.VoiceServerServiceConnectResponse_Welcome{Welcome: &pb.VoiceServerWelcome{}}})
	v.Logger.Info("voice server connected", zap.String("server", msg.ID), zap.String("endpoint", msg.Endpoint))
	actors.Reply(c, Registered{})
}

// serverDown ends the talk on a voice server that went away: its people
// lost their connection with it.
func (v *voice) serverDown(c *actor.Context, key string) {
	s := v.servers[key]
	if s == nil {
		return
	}
	delete(v.servers, key)
	for _, st := range v.states {
		if st.server == key {
			v.remove(st, "")
		}
	}
	for hash, p := range v.pending {
		if p.server == key {
			v.forget(hash, p)
		}
	}
	for id, rm := range v.rooms {
		if rm.server == key {
			delete(v.rooms, id)
		}
	}
	c.Engine().Poison(s.pid)
	v.Logger.Info("voice server disconnected", zap.String("server", key))
}

// send queues a message for a voice server, dropping one that fell too far
// behind: its people reconnect.
func (v *voice) send(s *server, resp *pb.VoiceServerServiceConnectResponse) {
	if s.queued.Load() >= v.Cfg.ServerQueueLimit {
		v.Logger.Warn("voice server is too slow, dropping it", zap.String("server", s.key))
		closeStream(s.stream)
		return
	}
	s.queued.Add(1)
	v.Engine.Send(s.pid, resp)
}

func closeStream(s Stream) {
	go func() { _ = s.Close() }()
}

// writer owns the sending half of one voice server's stream.
type writer struct {
	logger *zap.Logger
	key    string
	stream Stream
	queued *atomic.Int64
	failed bool
}

func newWriter(logger *zap.Logger, key string, stream Stream, queued *atomic.Int64) actor.Producer {
	return func() actor.Receiver {
		return &writer{logger: logger, key: key, stream: stream, queued: queued}
	}
}

func (w *writer) Receive(c *actor.Context) {
	resp, ok := c.Message().(*pb.VoiceServerServiceConnectResponse)
	if !ok {
		return
	}
	w.queued.Add(-1)
	if w.failed {
		return
	}
	if err := w.stream.Send(resp); err != nil {
		w.failed = true
		w.logger.Warn("failed to send to a voice server", zap.String("server", w.key), zap.Error(err))
		_ = w.stream.Close()
	}
}
