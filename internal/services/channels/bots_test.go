package channels

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/callbacks"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/updates"
)

// pushes records what a channel hands the sequencer, the callbacks and the
// hub.
type pushes struct {
	mu      sync.Mutex
	got     []updates.Push
	pending []callbacks.Pending
	members []hub.Members
}

func (p *pushes) take() []updates.Push {
	time.Sleep(50 * time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.got
	p.got = nil
	return out
}

// botChannel is a channel with a person and a bot in it.
func botChannel(t *testing.T, kind models.ChannelKind, seesAll bool) (*channel, *messageRepo, *pushes, gocql.UUID, gocql.UUID) {
	t.Helper()
	a, repo, _, person := newLoggedChannel(t, kind)
	seen := &pushes{}
	a.Updates = a.Engine.SpawnFunc(func(c *actor.Context) {
		if p, ok := c.Message().(updates.Push); ok {
			seen.mu.Lock()
			seen.got = append(seen.got, p)
			seen.mu.Unlock()
		}
	}, "updates")
	a.Callbacks = a.Engine.SpawnFunc(func(c *actor.Context) {
		if p, ok := c.Message().(callbacks.Pending); ok {
			seen.mu.Lock()
			seen.pending = append(seen.pending, p)
			seen.mu.Unlock()
		}
	}, "callbacks")
	a.Hub = a.Engine.SpawnFunc(func(c *actor.Context) {
		if d, ok := c.Message().(hub.Deliver); ok {
			seen.mu.Lock()
			seen.members = append(seen.members, d.Members)
			seen.mu.Unlock()
		}
	}, "hub")
	bot := ids.New()
	a.members[bot] = models.ChannelRoleMember
	a.bots = map[gocql.UUID]*botMember{bot: {seesAll: seesAll, username: "helper_bot"}}
	a.snapshot()
	return a, repo, seen, person, bot
}

func posts(t *testing.T, got []updates.Push, bot gocql.UUID) []string {
	t.Helper()
	var out []string
	for _, p := range got {
		if len(p.UserIDs) != 1 || p.UserIDs[0] != bot {
			t.Fatalf("pushed to %v", p.UserIDs)
		}
		switch e := p.Event.Payload.(type) {
		case *pb.Event_MessagePosted:
			out = append(out, e.MessagePosted.Message.Content)
		case *pb.Event_ChannelUpdated:
			out = append(out, "updated")
		case *pb.Event_PinsChanged:
			out = append(out, "pins")
		}
	}
	return out
}

func TestBotInPrivacyModeGetsWhatSpeaksToIt(t *testing.T) {
	a, repo, seen, person, bot := botChannel(t, models.ChannelPublic, false)
	for _, text := range []string{"hello all", "/weather Tehran", "thanks @helper_bot!", "ask @other_bot"} {
		say(a, repo, person, text)
	}
	say(a, repo, bot, "/my own command")
	a.broadcast(&pb.Event{Payload: &pb.Event_ChannelUpdated{ChannelUpdated: &pb.ChannelUpdated{Channel: &pb.Channel{}}}})
	a.pinsChanged()
	got := posts(t, seen.take(), bot)
	want := []string{"/weather Tehran", "thanks @helper_bot!", "updated"}
	if len(got) != len(want) {
		t.Fatalf("the bot got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the bot got %q, want %q", got, want)
		}
	}
	// Members get the channel's updates; the bot gets its own instead.
	seen.mu.Lock()
	defer seen.mu.Unlock()
	for _, m := range seen.members {
		if _, in := m[bot.String()]; in {
			t.Fatal("the bot was among the channel's recipients")
		}
		if _, in := m[person.String()]; !in {
			t.Fatal("the person was not among the channel's recipients")
		}
	}
}

func TestBotRepliesInItsThreads(t *testing.T) {
	a, repo, seen, person, bot := botChannel(t, models.ChannelPublic, false)
	root := say(a, repo, bot, "/ask me")
	other := say(a, repo, person, "another thread")
	seen.take()
	for _, r := range []struct {
		root *models.Message
		text string
	}{{root, "in the bot's thread"}, {other, "elsewhere"}} {
		m := &models.Message{ChannelID: a.id, ID: ids.NewTime(), UserID: person, Content: r.text, RootID: r.root.ID}
		a.broadcast(&pb.Event{Payload: &pb.Event_MessagePosted{MessagePosted: &pb.MessagePosted{Message: convertMessage(m)}}}, m, r.root)
	}
	if got := posts(t, seen.take(), bot); len(got) != 1 || got[0] != "in the bot's thread" {
		t.Fatalf("the bot got %q", got)
	}
}

func TestBotThatSeesAllGetsEverything(t *testing.T) {
	a, repo, seen, person, bot := botChannel(t, models.ChannelPublic, true)
	say(a, repo, person, "hello all")
	a.pinsChanged()
	if got := posts(t, seen.take(), bot); len(got) != 2 {
		t.Fatalf("the bot got %q", got)
	}
}

func TestBotInAConversationGetsEverything(t *testing.T) {
	a, repo, seen, person, bot := botChannel(t, models.ChannelDirect, false)
	say(a, repo, person, "hi")
	if got := posts(t, seen.take(), bot); len(got) != 1 {
		t.Fatalf("the bot got %q", got)
	}
}

func TestBotInPrivacyModeReadsNoHistory(t *testing.T) {
	a, _, _, _, bot := botChannel(t, models.ChannelPublic, false)
	p := &models.Principal{UserID: bot, Role: models.SystemRoleUser, Bot: true}
	if err := a.canRead(p); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a bot in privacy mode read: %v", err)
	}
	a.bots[bot].seesAll = true
	if err := a.canRead(p); err != nil {
		t.Fatalf("a bot that sees all: %v", err)
	}
}

func TestBotInPrivacyModeReadsWhatItWasSent(t *testing.T) {
	a, repo, _, person, bot := botChannel(t, models.ChannelPublic, false)
	b := a.privacyBot(&models.Principal{UserID: bot, Bot: true})
	if b == nil {
		t.Fatal("no privacy-mode membership")
	}
	root := &models.Message{ChannelID: a.id, ID: ids.NewTime(), UserID: bot, Content: "Pick one"}
	repo.messages[root.ID] = root
	ctx := context.Background()
	for _, tc := range []struct {
		m    *models.Message
		sent bool
	}{
		{&models.Message{ID: ids.NewTime(), UserID: person, Content: "/weather"}, true},
		{&models.Message{ID: ids.NewTime(), UserID: person, Content: "hi @helper_bot"}, true},
		{&models.Message{ID: ids.NewTime(), UserID: person, Content: "just chatting"}, false},
		{&models.Message{ID: ids.NewTime(), UserID: person, Content: "the second", RootID: root.ID}, true},
		{root, true},
	} {
		if got := a.sentTo(ctx, bot, b, tc.m); got != tc.sent {
			t.Errorf("%q: sent %v, want %v", tc.m.Content, got, tc.sent)
		}
	}
	a.bots[bot].seesAll = true
	if a.privacyBot(&models.Principal{UserID: bot, Bot: true}) != nil {
		t.Fatal("a bot that sees all reads as one in privacy mode")
	}
}

func TestOnlyBotsPutButtons(t *testing.T) {
	ok := &pb.Markup{Rows: []*pb.ButtonRow{{Buttons: []*pb.Button{
		{Text: "Yes", Action: &pb.Button_CallbackData{CallbackData: "yes"}},
		{Text: "Docs", Action: &pb.Button_Url{Url: "https://example.com/docs"}},
	}}}}
	bot, person := &models.Principal{Bot: true}, &models.Principal{}
	if b, err := markupOf(bot, ok); err != nil || len(b) == 0 {
		t.Fatalf("a bot's buttons: %v", err)
	}
	if b, err := markupOf(person, nil); err != nil || b != nil {
		t.Fatalf("no buttons: %v", err)
	}
	if _, err := markupOf(person, ok); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a person's buttons: %v", err)
	}
	long := string(make([]byte, 65))
	for name, bad := range map[string]*pb.Button{
		"no action":   {Text: "x"},
		"no text":     {Text: " ", Action: &pb.Button_CallbackData{CallbackData: "x"}},
		"long data":   {Text: "x", Action: &pb.Button_CallbackData{CallbackData: long}},
		"script link": {Text: "x", Action: &pb.Button_Url{Url: "javascript:alert(1)"}},
		"credentials": {Text: "x", Action: &pb.Button_Url{Url: "https://user:pass@example.com"}},
	} {
		m := &pb.Markup{Rows: []*pb.ButtonRow{{Buttons: []*pb.Button{bad}}}}
		if _, err := markupOf(bot, m); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	rows := &pb.Markup{}
	for range 9 {
		rows.Rows = append(rows.Rows, ok.Rows[0])
	}
	if _, err := markupOf(bot, rows); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("nine rows: %v", err)
	}
}

func TestPressingAButtonAsksTheBot(t *testing.T) {
	a, repo, seen, person, bot := botChannel(t, models.ChannelPublic, false)
	markup, _ := proto.Marshal(&pb.Markup{Rows: []*pb.ButtonRow{{Buttons: []*pb.Button{{Text: "Yes", Action: &pb.Button_CallbackData{CallbackData: "yes"}}}}}})
	m := &models.Message{ChannelID: a.id, ID: ids.NewTime(), UserID: bot, Content: "Agree?", Markup: markup}
	repo.messages[m.ID] = m
	presser := &models.Principal{UserID: person, Role: models.SystemRoleUser}
	press := func(p *models.Principal, data string) (string, error) {
		return a.press(Press{Target: Target{P: p, ChannelID: a.id}, MessageID: m.ID.String(), Data: data})
	}
	id, err := press(presser, "yes")
	if err != nil || id == "" {
		t.Fatalf("press: %q, %v", id, err)
	}
	got := seen.take()
	if len(got) != 1 || got[0].UserIDs[0] != bot {
		t.Fatalf("pushed %v", got)
	}
	q := got[0].Event.GetCallbackQuery()
	if q.GetId() != id || q.GetData() != "yes" || q.GetFrom().GetId() != person.String() || q.GetMessage().GetMarkup() == nil {
		t.Fatalf("query %v", q)
	}
	seen.mu.Lock()
	if len(seen.pending) != 1 || seen.pending[0].QueryID != id || seen.pending[0].Bot != bot || seen.pending[0].Presser != person {
		t.Fatalf("pending %+v", seen.pending)
	}
	seen.mu.Unlock()
	if _, err := press(presser, "no"); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("pressing a button the message lacks: %v", err)
	}
	if _, err := press(&models.Principal{UserID: bot, Bot: true}, "yes"); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a bot pressed: %v", err)
	}
	delete(a.bots, bot)
	if _, err := press(presser, "yes"); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("pressing the button of a bot that left: %v", err)
	}
}
