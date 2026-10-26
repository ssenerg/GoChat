// Package hub routes real-time events to the web gateways serving their
// recipients, and tracks who is online.
package hub

import (
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/config"
)

type Config struct {
	// GatewayQueueLimit is how many events a gateway may lag behind before it
	// is dropped and has to reconnect.
	GatewayQueueLimit int64         `mapstructure:"gateway_queue_limit"`
	PresenceGrace     time.Duration `mapstructure:"presence_grace"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.GatewayQueueLimit < 16 {
		return errors.New("gateway_queue_limit must be at least 16")
	}
	if c.PresenceGrace < 0 {
		return errors.New("presence_grace cannot be negative")
	}
	return nil
}

// Stream is the sending half of a gateway's Connect stream.
type Stream interface {
	Send(*pb.ConnectResponse) error
	Close() error
}

type RegisterGateway struct {
	ID     string
	Stream Stream
}

type UnregisterGateway struct{ ID string }

type Attach struct {
	GatewayID string
	ConnID    uint64
	User      *pb.User
	SessionID string
	ExpiresAt time.Time
}

type Reject struct {
	GatewayID string
	ConnID    uint64
	Retry     bool
}

type Detach struct {
	GatewayID string
	ConnID    uint64
}

// Members is a set of users shared by the events sent to them: it is
// replaced, never modified.
type Members map[string]struct{}

// Deliver sends Event to the connections of the online users among UserIDs,
// or among Members. With Viewing it goes only to the connections showing
// that channel, and never to Except. Neither list may be modified after it
// is sent.
type Deliver struct {
	UserIDs []string
	Members Members
	Viewing string
	Except  string
	Event   *pb.Event
}

// View records the channel a connection shows, empty for none.
type View struct {
	GatewayID string
	ConnID    uint64
	ChannelID string
}

type Broadcast struct{ Event *pb.Event }

// About sends Event, about UserID, to the user's own connections and to
// those watching them.
type About struct {
	UserID string
	Event  *pb.Event
}

// Watch replaces the people a connection watches with those of UserIDs it
// watches already or the directory found its user acquainted with, Checked;
// a Watch older than the last, by Seq, is dropped.
type Watch struct {
	GatewayID string
	ConnID    uint64
	Seq       uint64
	UserIDs   []string
	Checked   []string
}

// ToCheck replies with Unchecked: the connection's user, empty when it has
// none, and those of UserIDs it does not watch yet, which a Watch must
// check first.
type ToCheck struct {
	GatewayID string
	ConnID    uint64
	UserIDs   []string
}

type Unchecked struct {
	UserID  string
	UserIDs []string
}

// WatchedBy replies with the people UserID's connections watch, and
// WatchersOf with the users whose connections watch UserID.
type (
	WatchedBy  struct{ UserID string }
	WatchersOf struct{ UserID string }
)

// Unwatch stops Watcher's connections watching Subjects.
type Unwatch struct {
	Watcher  string
	Subjects []string
}

// RevokeSessions closes the connections of SessionIDs, or with All every
// connection of UserID.
type RevokeSessions struct {
	UserID     string
	SessionIDs []string
	All        bool
}

// Online replies with the members who are not offline, and Statuses with
// the statuses of UserIDs.
type (
	Online   struct{ Members Members }
	Statuses struct{ UserIDs []string }
)

// SetStatus records the availability a connected user chose.
type SetStatus struct {
	UserID   string
	Status   pb.Status
	DNDUntil time.Time
}

type Registered struct{}

type (
	checkOffline struct{ userID string }
	dndEnded     struct {
		userID string
		until  time.Time
	}
)

type choice struct {
	status   pb.Status
	dndUntil time.Time
}

// revokedTTL is how long a revoked session stays refused, covering attaches
// authenticated just before the revocation.
const revokedTTL = time.Minute

type conn struct {
	userID    string
	sessionID string
	viewing   string
	// watches are the people the connection watches, as of watchSeq.
	watches  []string
	watchSeq uint64
}

type gateway struct {
	id      string
	pid     *actor.PID
	stream  Stream
	pending *atomic.Int64
	conns   map[uint64]*conn
}

// connRef names a connection of a gateway.
type connRef struct {
	gw *gateway
	id uint64
}

type hub struct {
	cfg      *Config
	logger   *zap.Logger
	gateways map[string]*gateway
	// online counts each user's connections per gateway.
	online map[string]map[string]int
	// announced holds the status last announced of each user not offline,
	// chosen the statuses connected users chose.
	announced map[string]pb.Status
	chosen    map[string]choice
	// revoked holds recently revoked session ids and when they were revoked.
	revoked map[string]time.Time
	// conns holds each user's connections, and viewers those showing each
	// channel.
	conns   map[string][]connRef
	viewers map[string]map[*gateway]map[uint64]struct{}
	// watchers holds the connections watching each user.
	watchers map[string]map[*gateway]map[uint64]struct{}
}

func New(logger *zap.Logger, cfg *Config) actor.Producer {
	return func() actor.Receiver {
		return &hub{
			cfg:       cfg,
			logger:    logger,
			gateways:  make(map[string]*gateway),
			online:    make(map[string]map[string]int),
			announced: make(map[string]pb.Status),
			chosen:    make(map[string]choice),
			revoked:   make(map[string]time.Time),
			conns:     make(map[string][]connRef),
			viewers:   make(map[string]map[*gateway]map[uint64]struct{}),
			watchers:  make(map[string]map[*gateway]map[uint64]struct{}),
		}
	}
}

func (h *hub) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case RegisterGateway:
		h.register(c, msg)
	case UnregisterGateway:
		h.unregister(c, msg.ID)
	case Attach:
		h.attach(c, msg)
	case Reject:
		if gw := h.gateways[msg.GatewayID]; gw != nil {
			h.reject(c, gw, msg.ConnID, msg.Retry)
		}
	case Detach:
		h.detach(c, msg)
	case Deliver:
		h.deliver(c, msg)
	case View:
		if gw := h.gateways[msg.GatewayID]; gw != nil {
			if cn := gw.conns[msg.ConnID]; cn != nil {
				h.view(gw, msg.ConnID, cn, msg.ChannelID)
			}
		}
	case Broadcast:
		resp := &pb.ConnectResponse{Frame: &pb.ConnectResponse_Event{Event: msg.Event}}
		for _, gw := range h.gateways {
			h.send(c, gw, resp)
		}
	case About:
		h.about(c, msg.UserID, msg.Event)
	case Watch:
		if gw := h.gateways[msg.GatewayID]; gw != nil {
			if cn := gw.conns[msg.ConnID]; cn != nil && msg.Seq > cn.watchSeq {
				cn.watchSeq = msg.Seq
				h.watch(gw, msg.ConnID, cn, within(msg.UserIDs, cn.watches, msg.Checked))
			}
		}
	case ToCheck:
		var out Unchecked
		if gw := h.gateways[msg.GatewayID]; gw != nil {
			if cn := gw.conns[msg.ConnID]; cn != nil {
				out.UserID = cn.userID
				out.UserIDs = without(msg.UserIDs, cn.watches)
			}
		}
		actors.Reply(c, out)
	case WatchedBy:
		var out []string
		for _, r := range h.conns[msg.UserID] {
			for _, u := range r.gw.conns[r.id].watches {
				if !slices.Contains(out, u) {
					out = append(out, u)
				}
			}
		}
		actors.Reply(c, out)
	case WatchersOf:
		var out []string
		for gw, conns := range h.watchers[msg.UserID] {
			for id := range conns {
				if u := gw.conns[id].userID; !slices.Contains(out, u) {
					out = append(out, u)
				}
			}
		}
		actors.Reply(c, out)
	case Unwatch:
		for _, r := range h.conns[msg.Watcher] {
			cn := r.gw.conns[r.id]
			kept := slices.DeleteFunc(slices.Clone(cn.watches), func(u string) bool { return slices.Contains(msg.Subjects, u) })
			h.watch(r.gw, r.id, cn, kept)
		}
	case RevokeSessions:
		h.revoke(c, msg)
	case Online:
		var online []string
		h.eachOnline(msg.Members, func(id string) {
			if h.status(id) != pb.Status_STATUS_OFFLINE {
				online = append(online, id)
			}
		})
		actors.Reply(c, online)
	case Statuses:
		out := make(map[string]pb.Status, len(msg.UserIDs))
		for _, id := range msg.UserIDs {
			if s := h.status(id); s != pb.Status_STATUS_OFFLINE {
				out[id] = s
			}
		}
		actors.Reply(c, out)
	case SetStatus:
		if len(h.online[msg.UserID]) > 0 {
			h.choose(c, msg.UserID, choice{status: msg.Status, dndUntil: msg.DNDUntil})
			h.announce(c, msg.UserID)
		}
	case dndEnded:
		if ch, ok := h.chosen[msg.userID]; ok && ch.status == pb.Status_STATUS_DND && ch.dndUntil.Equal(msg.until) {
			h.announce(c, msg.userID)
		}
	case checkOffline:
		if len(h.online[msg.userID]) == 0 {
			delete(h.online, msg.userID)
			delete(h.chosen, msg.userID)
			h.announce(c, msg.userID)
		}
	case actor.Stopped:
		for _, gw := range h.gateways {
			closeStream(gw.stream)
		}
	}
}

// closeStream closes a stream off the actor's goroutine: closing waits for a
// send in progress, which a stalled gateway can hold for long.
func closeStream(s Stream) {
	go func() { _ = s.Close() }()
}

func (h *hub) reject(c *actor.Context, gw *gateway, connID uint64, retry bool) {
	h.send(c, gw, &pb.ConnectResponse{Frame: &pb.ConnectResponse_AttachRejected{AttachRejected: &pb.AttachRejected{ConnId: connID, Retry: retry}}})
}

func (h *hub) register(c *actor.Context, msg RegisterGateway) {
	if old := h.gateways[msg.ID]; old != nil {
		h.unregister(c, msg.ID)
	}
	gw := &gateway{
		id:      msg.ID,
		stream:  msg.Stream,
		pending: new(atomic.Int64),
		conns:   make(map[uint64]*conn),
	}
	gw.pid = c.SpawnChild(newWriter(h.logger, gw.id, msg.Stream, gw.pending), "gateway",
		actor.WithMiddleware(actors.Recover(h.logger)))
	h.gateways[msg.ID] = gw
	h.send(c, gw, &pb.ConnectResponse{Frame: &pb.ConnectResponse_Welcome{Welcome: &pb.Welcome{}}})
	h.logger.Info("gateway registered", zap.String("gateway", msg.ID))
	actors.Reply(c, Registered{})
}

func (h *hub) unregister(c *actor.Context, id string) {
	gw := h.gateways[id]
	if gw == nil {
		return
	}
	delete(h.gateways, id)
	for connID := range gw.conns {
		h.removeConn(c, gw, connID)
	}
	c.Engine().Poison(gw.pid)
	h.logger.Info("gateway unregistered", zap.String("gateway", id))
}

func (h *hub) attach(c *actor.Context, msg Attach) {
	gw := h.gateways[msg.GatewayID]
	if gw == nil {
		return
	}
	if _, ok := h.revoked[msg.SessionID]; ok {
		h.reject(c, gw, msg.ConnID, false)
		return
	}
	if _, dup := gw.conns[msg.ConnID]; dup {
		h.removeConn(c, gw, msg.ConnID)
	}
	userID := msg.User.GetId()
	gw.conns[msg.ConnID] = &conn{userID: userID, sessionID: msg.SessionID}
	h.conns[userID] = append(h.conns[userID], connRef{gw: gw, id: msg.ConnID})
	per := h.online[userID]
	if per == nil {
		per = make(map[string]int)
		h.online[userID] = per
	}
	per[gw.id]++
	h.send(c, gw, &pb.ConnectResponse{Frame: &pb.ConnectResponse_Attached{Attached: &pb.Attached{
		ConnId: msg.ConnID, User: msg.User, SessionId: msg.SessionID, ExpiresAt: timestamppb.New(msg.ExpiresAt),
	}}})
	// Bots show no presence.
	if msg.User.GetBot() {
		return
	}
	if _, ok := h.chosen[userID]; !ok {
		h.choose(c, userID, choice{status: msg.User.GetStatus(), dndUntil: msg.User.GetDndUntil().AsTime()})
	}
	h.announce(c, userID)
}

func (h *hub) choose(c *actor.Context, userID string, ch choice) {
	if ch.status != pb.Status_STATUS_DND || ch.dndUntil.Unix() <= 0 {
		ch.dndUntil = time.Time{}
	}
	h.chosen[userID] = ch
	if wait := time.Until(ch.dndUntil); wait > 0 {
		pid, engine, ended := c.PID(), c.Engine(), dndEnded{userID: userID, until: ch.dndUntil}
		time.AfterFunc(wait, func() { engine.Send(pid, ended) })
	}
}

// status is how available userID is now.
func (h *hub) status(userID string) pb.Status {
	if len(h.online[userID]) == 0 {
		return pb.Status_STATUS_OFFLINE
	}
	ch := h.chosen[userID]
	switch ch.status {
	case pb.Status_STATUS_AWAY, pb.Status_STATUS_OFFLINE:
		return ch.status
	case pb.Status_STATUS_DND:
		if ch.dndUntil.IsZero() || time.Now().Before(ch.dndUntil) {
			return ch.status
		}
	}
	return pb.Status_STATUS_ONLINE
}

func (h *hub) detach(c *actor.Context, msg Detach) {
	if gw := h.gateways[msg.GatewayID]; gw != nil {
		h.removeConn(c, gw, msg.ConnID)
	}
}

func (h *hub) removeConn(c *actor.Context, gw *gateway, connID uint64) {
	cn, ok := gw.conns[connID]
	if !ok {
		return
	}
	h.view(gw, connID, cn, "")
	h.watch(gw, connID, cn, nil)
	delete(gw.conns, connID)
	mine := slices.DeleteFunc(h.conns[cn.userID], func(r connRef) bool { return r.gw == gw && r.id == connID })
	if len(mine) == 0 {
		delete(h.conns, cn.userID)
	} else {
		h.conns[cn.userID] = mine
	}
	per := h.online[cn.userID]
	if per == nil {
		return
	}
	if per[gw.id]--; per[gw.id] <= 0 {
		delete(per, gw.id)
	}
	if len(per) == 0 {
		// The map stays, empty, until the grace period confirms the user left.
		pid, userID := c.PID(), cn.userID
		engine := c.Engine()
		time.AfterFunc(h.cfg.PresenceGrace, func() { engine.Send(pid, checkOffline{userID: userID}) })
	}
}

// view moves a connection to the channel it now shows.
func (h *hub) view(gw *gateway, connID uint64, cn *conn, channelID string) {
	if cn.viewing == channelID {
		return
	}
	if byGW := h.viewers[cn.viewing]; byGW != nil {
		if delete(byGW[gw], connID); len(byGW[gw]) == 0 {
			if delete(byGW, gw); len(byGW) == 0 {
				delete(h.viewers, cn.viewing)
			}
		}
	}
	cn.viewing = channelID
	if channelID == "" {
		return
	}
	byGW := h.viewers[channelID]
	if byGW == nil {
		byGW = make(map[*gateway]map[uint64]struct{})
		h.viewers[channelID] = byGW
	}
	if byGW[gw] == nil {
		byGW[gw] = make(map[uint64]struct{})
	}
	byGW[gw][connID] = struct{}{}
}

// within returns those of want, once each, that are in any of lists, and
// without those in none of them.
func within(want []string, lists ...[]string) []string  { return pick(want, lists, true) }
func without(want []string, lists ...[]string) []string { return pick(want, lists, false) }

func pick(want []string, lists [][]string, in bool) []string {
	listed := make(map[string]bool)
	for _, list := range lists {
		for _, u := range list {
			listed[u] = true
		}
	}
	seen := make(map[string]bool, len(want))
	out := make([]string, 0, len(want))
	for _, u := range want {
		if !seen[u] && listed[u] == in {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// watch replaces the people a connection watches.
func (h *hub) watch(gw *gateway, connID uint64, cn *conn, users []string) {
	for _, u := range cn.watches {
		if byGW := h.watchers[u]; byGW != nil {
			if delete(byGW[gw], connID); len(byGW[gw]) == 0 {
				if delete(byGW, gw); len(byGW) == 0 {
					delete(h.watchers, u)
				}
			}
		}
	}
	cn.watches = users
	for _, u := range users {
		byGW := h.watchers[u]
		if byGW == nil {
			byGW = make(map[*gateway]map[uint64]struct{})
			h.watchers[u] = byGW
		}
		if byGW[gw] == nil {
			byGW[gw] = make(map[uint64]struct{})
		}
		byGW[gw][connID] = struct{}{}
	}
}

// about sends an event about userID to their own connections and those
// watching them, and no one else.
func (h *hub) about(c *actor.Context, userID string, event *pb.Event) {
	byGateway := make(map[*gateway][]uint64)
	for _, r := range h.conns[userID] {
		byGateway[r.gw] = append(byGateway[r.gw], r.id)
	}
	for gw, conns := range h.watchers[userID] {
		for id := range conns {
			if gw.conns[id].userID != userID {
				byGateway[gw] = append(byGateway[gw], id)
			}
		}
	}
	for gw, ids := range byGateway {
		ev := shallowEvent(event)
		ev.ConnIds = ids
		h.send(c, gw, &pb.ConnectResponse{Frame: &pb.ConnectResponse_Event{Event: ev}})
	}
}

// eachOnline calls fn for the members with a connection, walking the
// smaller of the members and the users online.
func (h *hub) eachOnline(members Members, fn func(userID string)) {
	if len(h.online) < len(members) {
		for id := range h.online {
			if _, ok := members[id]; ok {
				fn(id)
			}
		}
		return
	}
	for id := range members {
		if _, ok := h.online[id]; ok {
			fn(id)
		}
	}
}

func (h *hub) deliver(c *actor.Context, msg Deliver) {
	for gw, conns := range h.route(msg) {
		ev := shallowEvent(msg.Event)
		ev.ConnIds = conns
		h.send(c, gw, &pb.ConnectResponse{Frame: &pb.ConnectResponse_Event{Event: ev}})
	}
}

// route finds the connections an event goes to, by gateway.
func (h *hub) route(msg Deliver) map[*gateway][]uint64 {
	byGateway := make(map[*gateway][]uint64, len(h.gateways))
	add := func(userID string, refs []connRef) {
		if userID == msg.Except {
			return
		}
		for _, r := range refs {
			byGateway[r.gw] = append(byGateway[r.gw], r.id)
		}
	}
	switch {
	case msg.Viewing != "":
		for gw, conns := range h.viewers[msg.Viewing] {
			for id := range conns {
				uid := gw.conns[id].userID
				if _, ok := msg.Members[uid]; ok && uid != msg.Except {
					byGateway[gw] = append(byGateway[gw], id)
				}
			}
		}
	case msg.Members != nil && len(h.conns) < len(msg.Members):
		for uid, refs := range h.conns {
			if _, ok := msg.Members[uid]; ok {
				add(uid, refs)
			}
		}
	case msg.Members != nil:
		for uid := range msg.Members {
			add(uid, h.conns[uid])
		}
	default:
		for _, uid := range msg.UserIDs {
			add(uid, h.conns[uid])
		}
	}
	return byGateway
}

// shallowEvent copies an event's envelope so each gateway gets its own
// recipient list while the payload is shared, read-only.
func shallowEvent(e *pb.Event) *pb.Event {
	return &pb.Event{Broadcast: e.Broadcast, Payload: e.Payload, Pts: e.Pts, PtsCount: e.PtsCount, PtsChannelId: e.PtsChannelId}
}

func (h *hub) revoke(c *actor.Context, msg RevokeSessions) {
	if msg.All {
		msg.SessionIDs = slices.Clone(msg.SessionIDs)
		for gwID := range h.online[msg.UserID] {
			gw := h.gateways[gwID]
			if gw == nil {
				continue
			}
			for _, cn := range gw.conns {
				if cn.userID == msg.UserID {
					msg.SessionIDs = append(msg.SessionIDs, cn.sessionID)
				}
			}
		}
		slices.Sort(msg.SessionIDs)
		msg.SessionIDs = slices.Compact(msg.SessionIDs)
	}
	if len(msg.SessionIDs) == 0 {
		return
	}
	now := time.Now()
	for id, at := range h.revoked {
		if now.Sub(at) > revokedTTL {
			delete(h.revoked, id)
		}
	}
	for _, id := range msg.SessionIDs {
		h.revoked[id] = now
	}
	resp := &pb.ConnectResponse{Frame: &pb.ConnectResponse_SessionsRevoked{SessionsRevoked: &pb.SessionsRevoked{SessionIds: msg.SessionIDs}}}
	for gwID := range h.online[msg.UserID] {
		gw := h.gateways[gwID]
		if gw == nil {
			continue
		}
		h.send(c, gw, resp)
		for connID, cn := range gw.conns {
			if _, ok := h.revoked[cn.sessionID]; ok && cn.userID == msg.UserID {
				h.removeConn(c, gw, connID)
			}
		}
	}
}

// announce tells a user's status, when it changed, to the connections
// that show it.
func (h *hub) announce(c *actor.Context, userID string) {
	s := h.status(userID)
	if was, ok := h.announced[userID]; (ok && was == s) || (!ok && s == pb.Status_STATUS_OFFLINE) {
		return
	}
	if s == pb.Status_STATUS_OFFLINE {
		delete(h.announced, userID)
	} else {
		h.announced[userID] = s
	}
	h.about(c, userID, &pb.Event{Payload: &pb.Event_Presence{Presence: &pb.Presence{
		UserId: userID, Online: s != pb.Status_STATUS_OFFLINE, Status: s,
	}}})
}

// send queues resp for the gateway, dropping a gateway that has fallen too far
// behind: it reconnects and its pages resynchronize.
func (h *hub) send(c *actor.Context, gw *gateway, resp *pb.ConnectResponse) {
	if gw.pending.Load() >= h.cfg.GatewayQueueLimit {
		h.logger.Warn("gateway is too slow, dropping it", zap.String("gateway", gw.id))
		closeStream(gw.stream)
		h.unregister(c, gw.id)
		return
	}
	gw.pending.Add(1)
	c.Send(gw.pid, resp)
}

// writer owns the sending half of one gateway's stream.
type writer struct {
	logger  *zap.Logger
	id      string
	stream  Stream
	pending *atomic.Int64
	failed  bool
}

func newWriter(logger *zap.Logger, id string, stream Stream, pending *atomic.Int64) actor.Producer {
	return func() actor.Receiver {
		return &writer{logger: logger, id: id, stream: stream, pending: pending}
	}
}

func (w *writer) Receive(c *actor.Context) {
	resp, ok := c.Message().(*pb.ConnectResponse)
	if !ok {
		return
	}
	w.pending.Add(-1)
	if w.failed {
		return
	}
	if err := w.stream.Send(resp); err != nil {
		w.failed = true
		w.logger.Warn("failed to send to gateway", zap.String("gateway", w.id), zap.Error(err))
		_ = w.stream.Close()
	}
}
