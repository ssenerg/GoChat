// Package callbacks holds the button presses bots have yet to answer, and
// hands each answer to whoever pressed.
package callbacks

import (
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/hub"
)

// TTL is how long a bot has to answer a press.
const TTL = time.Minute

// Pending records a press of Bot's button by Presser, answerable until
// Expires.
type Pending struct {
	QueryID string
	Presser gocql.UUID
	Bot     gocql.UUID
	Expires time.Time
}

// Answer is Bot's answer to a press; it replies Done, or an error when
// the press is not Bot's or no longer waits.
type Answer struct {
	QueryID string
	Bot     *pb.User
	Text    string
	Alert   bool
}

type Done struct{}

type sweep struct{}

type registry struct {
	hub     *actor.PID
	pending map[string]Pending
	sweeper *actor.SendRepeater
}

func New(hubPID *actor.PID) actor.Producer {
	return func() actor.Receiver {
		return &registry{hub: hubPID, pending: make(map[string]Pending)}
	}
}

func (r *registry) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		s := c.SendRepeat(c.PID(), sweep{}, TTL)
		r.sweeper = &s
	case actor.Stopped:
		if r.sweeper != nil {
			r.sweeper.Stop()
		}
	case Pending:
		r.pending[msg.QueryID] = msg
	case Answer:
		p, ok := r.pending[msg.QueryID]
		if !ok || time.Now().After(p.Expires) || p.Bot.String() != msg.Bot.GetId() {
			actors.Reply(c, ierrors.NotFound("the press was answered or is too old"))
			return
		}
		delete(r.pending, msg.QueryID)
		// The answer is for the moment only: it is not numbered.
		c.Send(r.hub, hub.Deliver{UserIDs: []string{p.Presser.String()}, Event: &pb.Event{Payload: &pb.Event_CallbackAnswer{
			CallbackAnswer: &pb.CallbackAnswer{QueryId: msg.QueryID, Text: msg.Text, Alert: msg.Alert, Bot: msg.Bot},
		}}})
		actors.Reply(c, Done{})
	case sweep:
		now := time.Now()
		for id, p := range r.pending {
			if now.After(p.Expires) {
				delete(r.pending, id)
			}
		}
	}
}
