package channels

import (
	"bytes"
	"context"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/unfurl"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/hub"
)

// askPreview asks for the preview of m's first link, once m is posted.
func (a *channel) askPreview(m *models.Message) {
	if a.Previews == nil || m.NoLinkPreview {
		return
	}
	if link := unfurl.FirstLink(m.Content); link != "" {
		a.Previews.Preview(Unfurl{ChannelID: a.id, MessageID: m.ID, RootID: m.RootID, URL: link})
	}
}

// previewed stores the preview made for a message, if its link is still
// the message's first, and tells the members.
func (a *channel) previewed(msg Previewed) {
	ctx, cancel := a.ctx()
	defer cancel()
	var (
		m   *models.Message
		err error
	)
	if ids.IsZero(msg.RootID) {
		m, err = a.Repo.GetMessage(ctx, a.id, msg.MessageID)
	} else {
		m, err = a.Repo.GetReply(ctx, a.id, msg.RootID, msg.MessageID)
	}
	if err != nil {
		return
	}
	data, err := proto.Marshal(msg.Preview)
	if err != nil || m.NoLinkPreview || unfurl.FirstLink(m.Content) != msg.URL || bytes.Equal(data, m.LinkPreview) {
		return
	}
	if err := a.Repo.SetLinkPreview(ctx, a.id, m.RootID, m.ID, data, false); err != nil {
		a.Logger.Warn("failed to store a link preview", zap.String("channel", a.id.String()), zap.Error(err))
		return
	}
	m.LinkPreview = data
	if _, err := a.previewChanged(ctx, m); err != nil {
		a.Logger.Warn("failed to render a message with its preview", zap.String("channel", a.id.String()), zap.Error(err))
	}
}

func (a *channel) removePreview(msg RemovePreview) (*pb.Message, error) {
	role, err := a.member(msg.P)
	if err != nil {
		return nil, err
	}
	if !a.Authz.Can(authz.ObjMessage, authz.ActEditOwn, string(role), string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	if a.archived() {
		return nil, errArchived
	}
	if !a.poster(msg.P.UserID).Allow() {
		return nil, ierrors.ResourceExhausted("you are editing too fast")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	m, err := a.messageOf(ctx, msg.MessageID, msg.RootID)
	if err != nil {
		return nil, err
	}
	if m.UserID != msg.P.UserID {
		return nil, ierrors.PermissionDenied()
	}
	var out *pb.Message
	if m.NoLinkPreview {
		list, err := a.render(ctx, []*models.Message{m})
		if err != nil {
			return nil, err
		}
		out = list[0]
	} else {
		if err := a.Repo.SetLinkPreview(ctx, a.id, m.RootID, m.ID, nil, true); err != nil {
			return nil, err
		}
		m.LinkPreview, m.NoLinkPreview = nil, true
		if out, err = a.previewChanged(ctx, m); err != nil {
			return nil, err
		}
	}
	return out, a.markSaved(ctx, msg.P.UserID, []*pb.Message{out})
}

// previewChanged tells the channel's people that a message's preview came
// or went, as an edit of it. Bots are not told: their messages are not
// edited by it. It returns the message for the caller.
func (a *channel) previewChanged(ctx context.Context, m *models.Message) (*pb.Message, error) {
	list, err := a.render(ctx, []*models.Message{m})
	if err != nil {
		return nil, err
	}
	mine := proto.CloneOf(list[0])
	ev := &pb.Event{Payload: &pb.Event_MessageEdited{MessageEdited: &pb.MessageEdited{Message: list[0]}}}
	a.number(ev)
	if len(a.recipients) > 0 {
		a.Engine.Send(a.Hub, hub.Deliver{Members: a.recipients, Event: ev})
	}
	return mine, nil
}
