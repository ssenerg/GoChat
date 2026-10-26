package gateway

import (
	"github.com/anthdm/hollywood/actor"
)

// waiter attaches a session for as long as a request waits for its
// updates, and hands what the router sends it to the request: Ready once
// attached, Event, Rejected or Revoked.
type waiter struct {
	*Deps
	connID uint64
	token  string
	out    chan<- any
}

// NewWaiter attaches token's session as connection connID until stopped,
// sending what the router sends it to out, without blocking.
func NewWaiter(d *Deps, connID uint64, token string, out chan<- any) actor.Producer {
	return func() actor.Receiver {
		return &waiter{Deps: d, connID: connID, token: token, out: out}
	}
}

func (w *waiter) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		c.Send(w.Router, register{connID: w.connID, token: w.token, pid: c.PID()})
	case actor.Stopped:
		c.Send(w.Router, unregister{connID: w.connID})
	case Ready, Event, Rejected, Revoked:
		select {
		case w.out <- msg:
		default:
		}
	}
}
