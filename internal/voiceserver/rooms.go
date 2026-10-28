package voiceserver

import (
	"context"
	"slices"
	"strconv"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/actors"
)

// Messages for the rooms actor.
type (
	// disconnect ends one connection, as the chat server asks.
	disconnect struct {
		channelID, session, reason string
	}
	// dropAll ends every connection, when the chat server is lost: it
	// forgot their talk.
	dropAll struct{ reason string }
)

type roomEntry struct {
	pid   *actor.PID
	peers int
}

// rooms runs one room actor per voice channel with talk on this server,
// and stops it when its last peer leaves. Every peer joins through here,
// so a room is never stopped with a peer on its way in.
//
// A room is no child of it: as a parent stops, hollywood poisons each child
// again, even one already stopping, and loses that second pill, so the stop
// would wait on the room forever. Each room gets one pill, from here.
type rooms struct {
	logger *zap.Logger
	cfg    *Config
	media  *Media
	link   *actor.PID
	byID   map[string]*roomEntry
	// stopping are the rooms told to stop, each done once it has.
	stopping []context.Context
	seq      uint64
}

func newRooms(logger *zap.Logger, cfg *Config, media *Media, link *actor.PID) actor.Producer {
	return func() actor.Receiver {
		return &rooms{logger: logger, cfg: cfg, media: media, link: link, byID: make(map[string]*roomEntry)}
	}
}

func (r *rooms) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Stopped:
		for _, e := range r.byID {
			r.stop(c, e.pid)
		}
		for _, done := range r.stopping {
			<-done.Done()
		}
	case addPeer:
		e := r.byID[msg.channelID]
		if e == nil {
			r.seq++
			pid := c.Engine().Spawn(newRoom(r.logger, r.cfg, r.media, r.link, c.PID(), msg.channelID), "room",
				actor.WithID(msg.channelID+"-"+strconv.FormatUint(r.seq, 36)), actor.WithMiddleware(actors.Recover(r.logger)))
			e = &roomEntry{pid: pid}
			r.byID[msg.channelID] = e
		}
		e.peers++
		// The room answers whoever asked.
		c.Engine().SendWithSender(e.pid, msg, c.Sender())
	case peerGone:
		e := r.byID[msg.channelID]
		if e == nil {
			return
		}
		if e.peers--; e.peers <= 0 {
			delete(r.byID, msg.channelID)
			r.stop(c, e.pid)
		}
	case toRoom:
		if e := r.byID[msg.channelID]; e != nil {
			c.Send(e.pid, msg.msg)
		}
	case disconnect:
		if e := r.byID[msg.channelID]; e != nil {
			c.Send(e.pid, removePeer{session: msg.session, code: closeDisconnected, reason: msg.reason})
		}
	case dropAll:
		for _, e := range r.byID {
			c.Send(e.pid, dropPeers(msg))
		}
	}
}

func (r *rooms) stop(c *actor.Context, pid *actor.PID) {
	r.stopping = slices.DeleteFunc(r.stopping, func(done context.Context) bool { return done.Err() != nil })
	r.stopping = append(r.stopping, c.Engine().Poison(pid))
}

// dropPeers ends every connection of a room.
type dropPeers struct{ reason string }
