// Package updates numbers what happens to each account, as channels number
// what happens in them, and replays both to clients that were away.
package updates

import (
	"context"

	"github.com/gocql/gocql"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
)

// Difference is what a client missed: the updates after the number it
// asked from, oldest first, and the state after them.
type Difference struct {
	Updates []*pb.Event
	Pts     int64
	TooLong bool
	More    bool
}

// Messages reads the messages a log refers to.
type Messages interface {
	GetMessage(ctx context.Context, channelID, id gocql.UUID) (*models.Message, error)
	GetReply(ctx context.Context, channelID, rootID, id gocql.UUID) (*models.Message, error)
}

// Renderer renders a channel's messages as clients see them.
type Renderer func(ctx context.Context, channelID gocql.UUID, list []*models.Message) ([]*pb.Message, error)

// Entry is how ev is logged: a post, an edit or a button press with a
// reference to its message, which is read as it is when replayed; anything
// else as itself.
func Entry(ev *pb.Event) *scylla.LogEntry {
	var (
		e     = &scylla.LogEntry{Kind: models.UpdateEvent}
		m     *pb.Message
		shell *pb.Event
	)
	switch p := ev.Payload.(type) {
	case *pb.Event_MessagePosted:
		e.Kind, m = models.UpdatePosted, p.MessagePosted.GetMessage()
		shell = &pb.Event{Payload: &pb.Event_MessagePosted{MessagePosted: &pb.MessagePosted{
			TeamId: p.MessagePosted.TeamId, Conversation: p.MessagePosted.Conversation,
		}}}
	case *pb.Event_MessageEdited:
		e.Kind, m = models.UpdateEdited, p.MessageEdited.GetMessage()
		shell = &pb.Event{Payload: &pb.Event_MessageEdited{MessageEdited: &pb.MessageEdited{}}}
	case *pb.Event_CallbackQuery:
		q := p.CallbackQuery
		e.Kind, m = models.UpdatePress, q.GetMessage()
		shell = &pb.Event{Payload: &pb.Event_CallbackQuery{CallbackQuery: &pb.CallbackQuery{Id: q.Id, From: q.From, Data: q.Data}}}
	default:
		shell = &pb.Event{Payload: ev.Payload}
	}
	if m != nil {
		e.MessageID, _ = ids.ParseTime(m.Id)
		e.RootID, _ = ids.ParseTime(m.RootId)
		if ch, ok := ids.Parse(m.ChannelId); ok {
			e.ChannelID = ch
		}
	}
	e.Event, _ = proto.Marshal(shell)
	return e
}

// Read answers a difference from a log whose latest update is upTo: the
// entries after from, at most limit of them. A log missing the update
// right after from no longer reaches back that far.
func Read(ctx context.Context, from, upTo int64, limit int, fetch func(after, upTo int64, limit int) ([]*scylla.LogEntry, error),
	resolve func(ctx context.Context, entries []*scylla.LogEntry) ([]*pb.Event, error),
) (*Difference, error) {
	switch {
	case from == upTo:
		return &Difference{Pts: upTo}, nil
	case from > upTo || from < 0:
		return &Difference{Pts: upTo, TooLong: true}, nil
	}
	entries, err := fetch(from, upTo, limit)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 || entries[0].Pts != from+1 {
		return &Difference{Pts: upTo, TooLong: true}, nil
	}
	events, err := resolve(ctx, entries)
	if err != nil {
		return nil, err
	}
	last := entries[len(entries)-1].Pts
	return &Difference{Updates: events, Pts: last, More: last < upTo}, nil
}

// Resolver turns log entries back into events.
type Resolver struct {
	Messages Messages
	Render   Renderer
	// Channel is the log's channel, for a channel's log.
	Channel gocql.UUID
	// Shown reports whether the reader may see a message; nil shows all.
	Shown func(messageID, rootID gocql.UUID) bool
}

// Resolve turns entries back into events: references become their
// messages as they are now, and those deleted since are left out.
func (r *Resolver) Resolve(ctx context.Context, entries []*scylla.LogEntry) ([]*pb.Event, error) {
	type ref struct {
		channel, id, root gocql.UUID
	}
	out := make([]*pb.Event, 0, len(entries))
	refs := make([]ref, 0, len(entries))
	// The messages to read, once each, by channel.
	found := map[ref]*models.Message{}
	for _, e := range entries {
		ev := &pb.Event{}
		if err := proto.Unmarshal(e.Event, ev); err != nil || ev.Payload == nil {
			continue
		}
		channel := e.ChannelID
		if ids.IsZero(channel) {
			channel = r.Channel
		}
		ev.Pts, ev.PtsCount = e.Pts, 1
		if !ids.IsZero(r.Channel) {
			ev.PtsChannelId = r.Channel.String()
		}
		id, root := e.MessageID, e.RootID
		if ids.IsZero(id) {
			id, root = eventMessage(ev)
		}
		if r.Shown != nil && !ids.IsZero(id) && !r.Shown(id, root) {
			continue
		}
		key := ref{}
		if e.Kind != models.UpdateEvent {
			key = ref{channel: channel, id: id, root: root}
			if _, ok := found[key]; !ok {
				m, err := r.message(ctx, key.channel, key.id, key.root)
				if err != nil {
					return nil, err
				}
				found[key] = m
			}
			if found[key] == nil {
				continue
			}
		}
		out = append(out, ev)
		refs = append(refs, key)
	}
	byChannel := map[gocql.UUID][]*models.Message{}
	for key, m := range found {
		if m != nil {
			byChannel[key.channel] = append(byChannel[key.channel], m)
		}
	}
	rendered := map[gocql.UUID]*pb.Message{}
	for channel, list := range byChannel {
		views, err := r.Render(ctx, channel, list)
		if err != nil {
			return nil, err
		}
		for i, v := range views {
			rendered[list[i].ID] = v
		}
	}
	for i, ev := range out {
		if ids.IsZero(refs[i].id) {
			continue
		}
		m := proto.CloneOf(rendered[refs[i].id])
		switch p := ev.Payload.(type) {
		case *pb.Event_MessagePosted:
			p.MessagePosted.Message = m
		case *pb.Event_MessageEdited:
			p.MessageEdited.Message = m
		case *pb.Event_CallbackQuery:
			p.CallbackQuery.Message = m
		}
	}
	return out, nil
}

func (r *Resolver) message(ctx context.Context, channel, id, root gocql.UUID) (*models.Message, error) {
	var (
		m   *models.Message
		err error
	)
	if ids.IsZero(root) {
		m, err = r.Messages.GetMessage(ctx, channel, id)
	} else {
		m, err = r.Messages.GetReply(ctx, channel, root, id)
	}
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return nil, nil
	}
	return m, err
}

// eventMessage names the message an event that keeps itself is about.
func eventMessage(ev *pb.Event) (id, root gocql.UUID) {
	var rawID, rawRoot string
	switch p := ev.Payload.(type) {
	case *pb.Event_MessageDeleted:
		rawID, rawRoot = p.MessageDeleted.MessageId, p.MessageDeleted.RootId
	case *pb.Event_ReactionsChanged:
		rawID, rawRoot = p.ReactionsChanged.MessageId, p.ReactionsChanged.RootId
	case *pb.Event_CallbackQuery:
		rawID, rawRoot = p.CallbackQuery.GetMessage().GetId(), p.CallbackQuery.GetMessage().GetRootId()
	}
	id, _ = ids.ParseTime(rawID)
	root, _ = ids.ParseTime(rawRoot)
	return id, root
}
