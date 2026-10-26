package gateway

import (
	"strings"
	"testing"

	"github.com/coder/websocket"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/web/render"
)

// quietSession is a session whose frames stay in its outbox.
func quietSession(t *testing.T) *session {
	t.Helper()
	r, err := render.New(100)
	if err != nil {
		t.Fatal(err)
	}
	return &session{
		Deps: &Deps{Logger: zap.NewNop(), Cfg: testConfig(), Render: r},
		out:  newOutbox(1 << 20), user: &pb.User{Id: "me", Username: "me"},
		channelID: "here", sidebar: map[string]bool{"here": true, "there": true},
		mentions: map[string]int{}, unread: map[string]bool{}, muted: map[string]bool{},
		directs: map[string]bool{}, names: map[string]string{}, peers: map[string]bool{}, peerOf: map[string]string{},
		saved: map[string]bool{}, reads: map[string]string{}, presses: map[string]*press{},
	}
}

func (s *session) frames() []string {
	items, _, _ := s.out.take()
	out := make([]string, len(items))
	for i, b := range items {
		out[i] = string(b)
	}
	return out
}

// quiet reports whether the session queued no frame, without waiting.
func (s *session) quiet() bool {
	s.out.mu.Lock()
	defer s.out.mu.Unlock()
	return len(s.out.items) == 0
}

func posted(id, channel string) *pb.Event {
	return &pb.Event{Payload: &pb.Event_MessagePosted{MessagePosted: &pb.MessagePosted{Message: &pb.Message{
		Id: id, ChannelId: channel, Content: "hi", Author: &pb.User{Id: "someone", Username: "someone"},
	}}}}
}

func TestUnreadOnceUntilRead(t *testing.T) {
	s := quietSession(t)
	for _, id := range []string{"1", "2", "3"} {
		s.event(nil, posted(id, "there"))
	}
	if got := s.frames(); len(got) != 1 || !strings.Contains(got[0], `id="unread-there"`) {
		t.Fatalf("frames %q", got)
	}
	// Read, the channel is marked again by its next message.
	delete(s.unread, "there")
	s.event(nil, posted("4", "there"))
	if got := s.frames(); len(got) != 1 {
		t.Fatalf("frames after reading %q", got)
	}
}

func TestOtherTeamsConversationsStayThere(t *testing.T) {
	s := quietSession(t)
	s.teamID = "acme"
	s.sidebar["dm"], s.directs["dm"] = true, true
	ev := func(channel, team string, conversation bool, root string) *pb.Event {
		e := posted("1", channel)
		p := e.GetMessagePosted()
		p.TeamId, p.Conversation, p.Message.Content = team, conversation, "@me look"
		if root != "" {
			p.Message.RootId, p.ParticipantIds = root, []string{"me"}
		}
		return e
	}
	cases := []struct {
		name          string
		ev            *pb.Event
		notify, inbox bool
	}{
		{"this team's conversation", ev("dm", "acme", true, ""), true, false},
		{"another team's conversation", ev("other-dm", "beta", true, ""), false, false},
		{"another team's channel", ev("other", "beta", false, ""), true, false},
		{"a thread in this team", ev("there", "acme", false, "r"), true, true},
		{"a thread in another team's conversation", ev("other-dm", "beta", true, "r"), false, false},
		{"a thread in another team's channel", ev("other", "beta", false, "r"), true, false},
	}
	for _, c := range cases {
		s.event(nil, c.ev)
		// A closed outbox hands over what it holds without waiting.
		s.out.close(websocket.StatusNormalClosure)
		var notify, inbox bool
		for _, f := range s.frames() {
			notify = notify || strings.Contains(f, `id="notify"`)
			inbox = inbox || strings.Contains(f, `id="threads-unread"`)
		}
		if notify != c.notify || inbox != c.inbox {
			t.Errorf("%s: notified %v, marked the threads %v", c.name, notify, inbox)
		}
		s.out = newOutbox(1 << 20)
	}
}

func answer(id, text string) *pb.Event {
	return &pb.Event{Payload: &pb.Event_CallbackAnswer{CallbackAnswer: &pb.CallbackAnswer{
		QueryId: id, Text: text, Bot: &pb.User{Id: "bot", DisplayName: "Echo"},
	}}}
}

func TestAnswersShowOnlyInThePressingTab(t *testing.T) {
	s := quietSession(t)
	s.pressed("q1")
	s.event(nil, answer("q1", "thanks"))
	if got := s.frames(); len(got) != 1 || !strings.Contains(got[0], "Echo: thanks") {
		t.Fatalf("the answer: %q", got)
	}
	// An answer can beat the tab's own word of its press.
	s.event(nil, answer("q2", "fast"))
	if !s.quiet() {
		t.Fatalf("an answer shown before its press: %q", s.frames())
	}
	s.pressed("q2")
	if got := s.frames(); len(got) != 1 || !strings.Contains(got[0], "Echo: fast") {
		t.Fatalf("the early answer: %q", got)
	}
	// Another tab's press never shows here, and the held answers are few.
	for i := range 2 * maxPresses {
		s.event(nil, answer(strings.Repeat("x", i+1), "theirs"))
	}
	if !s.quiet() || len(s.presses) > maxPresses {
		t.Fatalf("another tab's answers shown, or %d held", len(s.presses))
	}
}
