package channels

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/blocks"
)

type blockList map[gocql.UUID][]gocql.UUID

func (b blockList) ListBlocks(_ context.Context, userID gocql.UUID, _ int) ([]*models.Block, error) {
	var out []*models.Block
	for _, id := range b[userID] {
		out = append(out, &models.Block{UserID: userID, BlockedID: id})
	}
	return out, nil
}

// mentionWrites records whom posts call out.
type mentionWrites struct {
	*writes
	mentioned chan []gocql.UUID
}

func (m *mentionWrites) AddMentions(_ context.Context, _, _ gocql.UUID, userIDs []gocql.UUID) error {
	m.mentioned <- slices.Clone(userIDs)
	return nil
}

func (m *mentionWrites) GetUserIDByUsername(context.Context, string) (gocql.UUID, error) {
	return gocql.UUID{}, ierrors.NotFound("not found")
}

// blockChannel is a channel of kind with people, whose blocks are list.
func blockChannel(t *testing.T, kind models.ChannelKind, list blockList, people ...gocql.UUID) (*channel, *mentionWrites) {
	t.Helper()
	a, _, _, _ := previewChannel(t)
	w := &mentionWrites{writes: a.Repo.(*writes), mentioned: make(chan []gocql.UUID, 4)}
	a.Repo, a.Previews, a.Blocks = w, nil, blocks.NewCache(list, 10, 10, time.Minute)
	a.ch.Kind = kind
	keys := make([]string, len(people))
	a.members = map[gocql.UUID]models.ChannelRole{}
	for i, id := range people {
		keys[i] = id.String()
		a.members[id] = models.ChannelRoleMember
	}
	if kind.Conversation() {
		slices.Sort(keys)
		a.ch.Name = keys[0]
		for _, k := range keys[1:] {
			a.ch.Name += ":" + k
		}
	}
	a.snapshot()
	return a, w
}

func TestBlockedPeopleCannotWriteInADirectConversation(t *testing.T) {
	me, peer, other := ids.New(), ids.New(), ids.New()
	post := func(a *channel, from gocql.UUID) error {
		_, err := a.post(Post{Target: Target{P: &models.Principal{UserID: from, Role: models.SystemRoleUser}, ChannelID: a.id}, Content: "hi"})
		return err
	}
	// The peer blocked me: I hear a refusal that does not say so, and they
	// hear they must unblock me first.
	a, _ := blockChannel(t, models.ChannelDirect, blockList{peer: {me}}, me, peer)
	if err := post(a, me); !ierrors.Is(err, ierrors.CodePermissionDenied) || ierrors.As(err).Message != ErrNotTaking.Message {
		t.Fatalf("someone blocked posted: %v", err)
	}
	if err := post(a, peer); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("someone posted to whom they blocked: %v", err)
	}
	// Blocking someone else changes nothing here.
	a, _ = blockChannel(t, models.ChannelDirect, blockList{peer: {other}}, me, peer)
	if err := post(a, me); err != nil {
		t.Fatal(err)
	}
	// A group conversation still takes their posts: the others read them.
	a, _ = blockChannel(t, models.ChannelGroup, blockList{peer: {me}}, me, peer, other)
	if err := post(a, me); err != nil {
		t.Fatalf("a group refused a post of someone one of its people blocked: %v", err)
	}
}

func TestBlockedPeopleCallOutNoOne(t *testing.T) {
	ann, bob, cat := ids.New(), ids.New(), ids.New()
	a, w := blockChannel(t, models.ChannelPublic, blockList{bob: {ann}}, ann, bob, cat)
	mentioned := func(from gocql.UUID, text string) []gocql.UUID {
		t.Helper()
		if _, err := a.post(Post{Target: Target{P: &models.Principal{UserID: from, Role: models.SystemRoleUser}, ChannelID: a.id}, Content: text}); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-w.mentioned:
			slices.SortFunc(got, func(x, y gocql.UUID) int { return compareIDs(x, y) })
			return got
		case <-time.After(5 * time.Second):
			t.Fatal("no mentions recorded")
			return nil
		}
	}
	// Bob blocked Ann: her @channel counts for Cat alone.
	if got := mentioned(ann, "@channel standup"); !slices.Equal(got, []gocql.UUID{cat}) {
		t.Fatalf("ann's @channel called out %v, want only cat %v", got, cat)
	}
	want := []gocql.UUID{ann, bob}
	slices.SortFunc(want, compareIDs)
	if got := mentioned(cat, "@channel standup"); !slices.Equal(got, want) {
		t.Fatalf("cat's @channel called out %v, want %v", got, want)
	}
}

func compareIDs(x, y gocql.UUID) int {
	return slices.Compare(x[:], y[:])
}

// deletes records the messages a delete takes.
type deletes struct {
	*mentionWrites
	repo    *messageRepo
	deleted []gocql.UUID
}

func (d *deletes) DeleteMessage(_ context.Context, _, id gocql.UUID) error {
	d.deleted = append(d.deleted, id)
	return nil
}

func (d *deletes) DeleteReactions(context.Context, gocql.UUID, gocql.UUID) error { return nil }
func (d *deletes) Unpin(context.Context, gocql.UUID, gocql.UUID) error           { return nil }

func TestAModeratedDeleteNeedsNoMembership(t *testing.T) {
	ann, bob, admin := ids.New(), ids.New(), &models.Principal{UserID: ids.New(), Role: models.SystemRoleUser}
	a, w := blockChannel(t, models.ChannelDirect, nil, ann, bob)
	repo := w.repo
	m := &models.Message{ChannelID: a.id, ID: ids.NewTime(), UserID: bob, Content: "rude"}
	repo.messages[m.ID] = m
	d := &deletes{mentionWrites: w, repo: repo}
	a.Repo = d
	// An admin outside the conversation cannot delete in it...
	if _, err := a.delete(Delete{Target: Target{P: admin, ChannelID: a.id}, MessageID: m.ID.String()}); err == nil {
		t.Fatal("someone outside a conversation deleted in it")
	}
	// ...but may for a report the safety service checked.
	if _, err := a.delete(Delete{Target: Target{P: admin, ChannelID: a.id}, MessageID: m.ID.String(), Moderated: true}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.deleted, []gocql.UUID{m.ID}) {
		t.Fatalf("deleted %v", d.deleted)
	}
}
