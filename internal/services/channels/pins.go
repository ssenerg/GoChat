package channels

import (
	"context"
	"slices"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/convert"
)

// maxPins bounds the messages a channel pins.
const maxPins = 50

// Pin pins a message for the channel, or with Pinned false unpins it; Save
// saves it for the caller alone. Both reply with the message. Pins replies
// with the pinned messages, newest first.
type (
	Pin struct {
		Target
		MessageID, RootID string
		Pinned            bool
	}
	Pins struct{ Target }
	Save struct {
		Target
		MessageID, RootID string
		Saved             bool
	}
)

func (a *channel) pin(msg Pin) (*pb.Message, error) {
	if _, err := a.member(msg.P); err != nil {
		return nil, err
	}
	if a.archived() {
		return nil, errArchived
	}
	if !a.poster(msg.P.UserID).Allow() {
		return nil, ierrors.ResourceExhausted("you are pinning too fast")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	m, err := a.messageOf(ctx, msg.MessageID, msg.RootID)
	if err != nil {
		return nil, err
	}
	if m.Pinned != msg.Pinned {
		if msg.Pinned && a.pins >= maxPins {
			return nil, ierrors.FailedPrecondition("a channel pins at most 50 messages: unpin one first")
		}
		if err := a.Repo.SetPinned(ctx, m, msg.Pinned, msg.P.UserID); err != nil {
			return nil, err
		}
		m.Pinned = msg.Pinned
		if msg.Pinned {
			a.pins++
		} else {
			a.pins = max(a.pins-1, 0)
		}
		a.pinsChanged()
	}
	out, err := a.render(ctx, []*models.Message{m})
	if err != nil {
		return nil, err
	}
	mine := proto.CloneOf(out[0])
	a.broadcast(&pb.Event{Payload: &pb.Event_MessageEdited{MessageEdited: &pb.MessageEdited{Message: out[0]}}}, m)
	return mine, a.markSaved(ctx, msg.P.UserID, []*pb.Message{mine})
}

func (a *channel) pinsChanged() {
	a.broadcast(&pb.Event{Payload: &pb.Event_PinsChanged{PinsChanged: &pb.PinsChanged{
		ChannelId: a.id.String(), Count: int32(a.pins), //nolint:gosec // at most maxPins
	}}})
}

// unpinGone forgets the pin of a deleted message.
func (a *channel) unpinGone(ctx context.Context, m *models.Message) {
	if !m.Pinned {
		return
	}
	if err := a.Repo.Unpin(ctx, a.id, m.ID); err != nil {
		a.Logger.Warn("failed to unpin a deleted message", zap.String("channel", a.id.String()), zap.Error(err))
	}
	a.pins = max(a.pins-1, 0)
	a.pinsChanged()
}

func (a *channel) listPins(c *actor.Context, msg Pins) error {
	if err := a.canRead(msg.P); err != nil {
		return err
	}
	userID, cleared := msg.P.UserID, a.cleared[msg.P.UserID]
	a.async(c, func(ctx context.Context) (any, error) {
		pins, err := a.Repo.ListPins(ctx, a.id, maxPins)
		if err != nil {
			return nil, err
		}
		found := make([]*models.Message, len(pins))
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(8)
		for i, p := range pins {
			g.Go(func() error {
				m, err := a.messageOf(gctx, p.MessageID.String(), convert.ID(p.RootID))
				if ierrors.Is(err, ierrors.CodeNotFound) {
					return nil
				}
				found[i] = m
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
		list := slices.DeleteFunc(found, func(m *models.Message) bool { return m == nil || !shown(m, cleared) })
		out, err := a.render(ctx, list)
		if err != nil {
			return nil, err
		}
		return out, a.markSaved(ctx, userID, out)
	})
	return nil
}

func (a *channel) save(msg Save) (*pb.Message, error) {
	if err := a.canRead(msg.P); err != nil {
		return nil, err
	}
	if !a.poster(msg.P.UserID).Allow() {
		return nil, ierrors.ResourceExhausted("you are saving too fast")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	m, err := a.messageOf(ctx, msg.MessageID, msg.RootID)
	if err != nil {
		return nil, err
	}
	if err := a.Repo.SetSaved(ctx, msg.P.UserID, m, a.ch.TeamID, msg.Saved); err != nil {
		return nil, err
	}
	a.deliver(msg.P.UserID, &pb.Event{Payload: &pb.Event_SavedChanged{SavedChanged: &pb.SavedChanged{
		ChannelId: a.id.String(), MessageId: m.ID.String(), RootId: convert.ID(m.RootID), Saved: msg.Saved,
	}}})
	out, err := a.render(ctx, []*models.Message{m})
	if err != nil {
		return nil, err
	}
	out[0].Saved = msg.Saved
	return out[0], nil
}

// markSaved notes which of the messages userID saved.
func (a *channel) markSaved(ctx context.Context, userID gocql.UUID, list []*pb.Message) error {
	msgIDs := make([]gocql.UUID, 0, len(list))
	for _, m := range list {
		if id, ok := ids.ParseTime(m.Id); ok {
			msgIDs = append(msgIDs, id)
		}
	}
	saved, err := a.Repo.SavedAmong(ctx, userID, msgIDs)
	if err != nil {
		return err
	}
	for _, m := range list {
		if id, ok := ids.ParseTime(m.Id); ok {
			m.Saved = saved[id]
		}
	}
	return nil
}
