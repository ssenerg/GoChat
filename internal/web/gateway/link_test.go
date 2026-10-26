package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// fakeCalls answers a link's calls: the account's state, its difference
// and an echo.
type fakeCalls struct {
	mu    sync.Mutex
	state int64
	// diffs answers each difference in turn; gate holds them until closed.
	diffs []*pb.GetDifferenceResponse
	gate  chan struct{}
	asked []int64
	who   []drpc.Call
}

func (f *fakeCalls) Allow(_ drpc.Call, method string, anonymous bool) error {
	if anonymous && method != "gochat.v1.AuthService/Login" {
		return ierrors.Unauthenticated()
	}
	return nil
}

func (f *fakeCalls) Invoke(_ context.Context, call drpc.Call, method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.who = append(f.who, call)
	gate := f.gate
	f.mu.Unlock()
	switch method {
	case "gochat.v1.UpdatesService/GetState":
		f.mu.Lock()
		defer f.mu.Unlock()
		return proto.Marshal(&pb.GetStateResponse{Pts: f.state})
	case "gochat.v1.UpdatesService/GetDifference":
		if gate != nil {
			<-gate
		}
		var req pb.GetDifferenceRequest
		_ = proto.Unmarshal(payload, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.asked = append(f.asked, req.Pts)
		if len(f.diffs) == 0 {
			return proto.Marshal(&pb.GetDifferenceResponse{Pts: req.Pts})
		}
		d := f.diffs[0]
		f.diffs = f.diffs[1:]
		return proto.Marshal(d)
	case "test/Echo":
		return payload, nil
	case "test/Fail":
		return nil, ierrors.NotFound("nothing here")
	}
	return nil, errors.New("unexpected call " + method)
}

type linkHarness struct {
	t      *testing.T
	engine *actor.Engine
	router *inbox
	link   *actor.PID
	client *websocket.Conn
	frames chan *pb.Down
	end    chan error
}

func newLinkHarness(t *testing.T, calls *fakeCalls) *linkHarness {
	t.Helper()
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	deps := appDeps(1 << 20)
	h := &linkHarness{t: t, engine: engine, router: newInbox(engine, "router")}
	deps.Router = h.router.pid
	pids := make(chan *actor.PID, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		in := NewInflow(deps.Cfg)
		pid := engine.Spawn(NewLink(deps, calls, 1, drpc.Call{ClientIP: "10.0.0.1"}, conn, in), "link")
		pids <- pid
		defer engine.Poison(pid)
		for {
			typ, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary && in.Admit(len(data)) {
				engine.Send(pid, Frame{Data: data})
			}
		}
	}))
	t.Cleanup(srv.Close)
	h.client, _, err = websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.client.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = h.client.CloseNow() })
	h.link = <-pids
	h.frames, h.end = make(chan *pb.Down, 64), make(chan error, 1)
	go func() {
		for {
			_, data, err := h.client.Read(context.Background())
			if err != nil {
				h.end <- err
				return
			}
			d := &pb.Down{}
			if err := proto.Unmarshal(data, d); err != nil {
				t.Errorf("not a Down frame: %v", err)
				continue
			}
			h.frames <- d
		}
	}()
	return h
}

func (h *linkHarness) up(u *pb.Up) {
	h.t.Helper()
	b, _ := proto.Marshal(u)
	if err := h.client.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		h.t.Fatal(err)
	}
}

func (h *linkHarness) hello(token string, pts int64) {
	h.t.Helper()
	h.up(&pb.Up{Frame: &pb.Up_Hello{Hello: &pb.SocketHello{Token: token, Layer: Layer, Pts: pts, Client: "test/1"}}})
}

func (h *linkHarness) next() *pb.Down {
	h.t.Helper()
	select {
	case d := <-h.frames:
		return d
	case err := <-h.end:
		h.t.Fatalf("the socket ended: %v", err)
	case <-time.After(2 * time.Second):
		h.t.Fatal("no frame")
	}
	return nil
}

func (h *linkHarness) quiet() {
	h.t.Helper()
	select {
	case d := <-h.frames:
		h.t.Fatalf("an unexpected frame: %v", d)
	case <-time.After(100 * time.Millisecond):
	}
}

func (h *linkHarness) closed(code websocket.StatusCode) {
	h.t.Helper()
	for {
		select {
		case <-h.frames:
		case err := <-h.end:
			if got := websocket.CloseStatus(err); got != code {
				h.t.Fatalf("closed with %d, want %d: %v", got, code, err)
			}
			return
		case <-time.After(2 * time.Second):
			h.t.Fatalf("the socket stayed open, want %d", code)
		}
	}
}

// attach plays the router: the link registers its token and is attached.
func (h *linkHarness) attach(token string) {
	h.t.Helper()
	reg, ok := h.router.expect(h.t, time.Second).(register)
	if !ok || reg.token != token {
		h.t.Fatalf("the link registered %+v", reg)
	}
	h.engine.Send(h.link, Ready{User: &pb.User{Id: "me"}, SessionID: "s1", ExpiresAt: time.Now().Add(time.Hour)})
}

func account(pts int64) *pb.Event {
	return &pb.Event{Pts: pts, PtsCount: 1, Payload: &pb.Event_ChannelMuted{ChannelMuted: &pb.ChannelMuted{ChannelId: "c"}}}
}

func updatePts(t *testing.T, d *pb.Down) []int64 {
	t.Helper()
	u := d.GetUpdates()
	if u == nil {
		t.Fatalf("not updates: %v", d)
	}
	var out []int64
	for _, ev := range u.Updates {
		out = append(out, ev.Pts)
	}
	return out
}

func TestFreshSocketStartsFromNow(t *testing.T) {
	calls := &fakeCalls{state: 5}
	h := newLinkHarness(t, calls)
	h.hello("tok", 0)
	h.attach("tok")
	w := h.next().GetWelcome()
	if w == nil || w.Pts != 5 || w.Reload || w.User.GetId() != "me" || w.SessionId != "s1" || w.Layer != Layer {
		t.Fatalf("welcome %v", w)
	}
	h.engine.Send(h.link, Event{Event: account(5)})
	h.engine.Send(h.link, Event{Event: account(6)})
	h.engine.Send(h.link, Event{Event: &pb.Event{Payload: &pb.Event_Typing{Typing: &pb.Typing{ChannelId: "c"}}}})
	h.engine.Send(h.link, Event{Event: &pb.Event{Pts: 9, PtsCount: 1, PtsChannelId: "c", Payload: &pb.Event_PinsChanged{PinsChanged: &pb.PinsChanged{}}}})
	if got := updatePts(t, h.next()); len(got) != 1 || got[0] != 6 {
		t.Fatalf("the update already had was sent again: %v", got)
	}
	if h.next().GetEphemeral().GetTyping() == nil {
		t.Fatal("typing did not come as ephemeral")
	}
	if d := h.next(); d.GetUpdates().GetUpdates()[0].PtsChannelId != "c" {
		t.Fatalf("a channel's update: %v", d)
	}
	h.quiet()
	if c := calls.who[0]; c.Session != "tok" || c.UserAgent != "test/1" || c.ClientIP != "10.0.0.1" {
		t.Fatalf("calls made as %+v", c)
	}
}

func TestSocketCatchesUpBeforeNewUpdates(t *testing.T) {
	calls := &fakeCalls{gate: make(chan struct{}), diffs: []*pb.GetDifferenceResponse{
		{Updates: []*pb.Event{account(4), account(5)}, Pts: 5, More: true},
		{Updates: []*pb.Event{account(6)}, Pts: 6},
	}}
	h := newLinkHarness(t, calls)
	h.hello("tok", 3)
	h.attach("tok")
	// New updates arrive while the socket catches up: those the replay
	// holds are dropped, the rest wait for it.
	h.engine.Send(h.link, Event{Event: account(6)})
	h.engine.Send(h.link, Event{Event: account(7)})
	time.Sleep(50 * time.Millisecond)
	close(calls.gate)
	w := h.next().GetWelcome()
	if w == nil || w.Pts != 3 || w.Reload {
		t.Fatalf("welcome %v", w)
	}
	var got []int64
	for len(got) < 4 {
		got = append(got, updatePts(t, h.next())...)
	}
	if want := []int64{4, 5, 6, 7}; len(got) != 4 || got[0] != 4 || got[2] != 6 || got[3] != 7 {
		t.Fatalf("updates %v, want %v", got, want)
	}
	h.quiet()
	if len(calls.asked) != 2 || calls.asked[0] != 3 || calls.asked[1] != 5 {
		t.Fatalf("differences asked from %v", calls.asked)
	}
}

func TestSocketTooFarBehindReloads(t *testing.T) {
	calls := &fakeCalls{diffs: []*pb.GetDifferenceResponse{{Pts: 40, TooLong: true}}}
	h := newLinkHarness(t, calls)
	h.hello("tok", 3)
	h.attach("tok")
	if w := h.next().GetWelcome(); w == nil || !w.Reload || w.Pts != 40 {
		t.Fatalf("welcome %v", w)
	}
}

func TestSocketResumesAfterTheServerReconnects(t *testing.T) {
	calls := &fakeCalls{state: 2}
	h := newLinkHarness(t, calls)
	h.hello("tok", 0)
	h.attach("tok")
	h.next()
	h.engine.Send(h.link, Event{Event: account(3)})
	h.next()
	calls.mu.Lock()
	calls.diffs = []*pb.GetDifferenceResponse{{Updates: []*pb.Event{account(4)}, Pts: 4}}
	calls.mu.Unlock()
	h.engine.Send(h.link, Ready{User: &pb.User{Id: "me"}, SessionID: "s1", Again: true})
	if w := h.next().GetWelcome(); w == nil || w.Pts != 3 || w.Reload {
		t.Fatalf("welcome again %v", w)
	}
	if got := updatePts(t, h.next()); len(got) != 1 || got[0] != 4 {
		t.Fatalf("replayed %v", got)
	}
	if calls.asked[0] != 3 {
		t.Fatalf("resumed from %d, want 3", calls.asked[0])
	}
}

func TestSocketCalls(t *testing.T) {
	calls := &fakeCalls{}
	h := newLinkHarness(t, calls)
	h.hello("tok", 0)
	h.attach("tok")
	h.next()
	h.up(&pb.Up{Frame: &pb.Up_Call{Call: &pb.Call{Id: 7, Method: "test/Echo", Payload: []byte("hi")}}})
	h.up(&pb.Up{Frame: &pb.Up_Call{Call: &pb.Call{Id: 8, Method: "test/Fail"}}})
	h.up(&pb.Up{Frame: &pb.Up_Ping{Ping: &pb.Ping{Id: 3}}})
	results := map[uint64]*pb.Result{}
	pong := false
	for len(results) < 2 || !pong {
		d := h.next()
		if r := d.GetResult(); r != nil {
			results[r.Id] = r
		}
		if d.GetPong().GetId() == 3 {
			pong = true
		}
	}
	if string(results[7].GetPayload()) != "hi" {
		t.Fatalf("echo %v", results[7])
	}
	if e := results[8].GetError(); e.GetCode() != uint32(ierrors.CodeNotFound) || e.GetMessage() != "nothing here" {
		t.Fatalf("failure %v", results[8])
	}
}

func TestAnonymousSocketSignsInThenSaysHelloAgain(t *testing.T) {
	calls := &fakeCalls{}
	h := newLinkHarness(t, calls)
	h.hello("", 0)
	if w := h.next().GetWelcome(); w == nil || w.User != nil {
		t.Fatalf("anonymous welcome %v", w)
	}
	h.up(&pb.Up{Frame: &pb.Up_Call{Call: &pb.Call{Id: 1, Method: "test/Echo"}}})
	if e := h.next().GetResult().GetError(); e.GetCode() != uint32(ierrors.CodeUnauthenticated) {
		t.Fatalf("an anonymous call: %v", e)
	}
	h.hello("tok", 0)
	h.attach("tok")
	if w := h.next().GetWelcome(); w.GetUser().GetId() != "me" {
		t.Fatalf("welcome %v", w)
	}
	// A signed-in socket says hello once.
	h.hello("other", 0)
	h.closed(CloseProtocol)
}

func TestSocketWantsHelloFirst(t *testing.T) {
	h := newLinkHarness(t, &fakeCalls{})
	h.up(&pb.Up{Frame: &pb.Up_Call{Call: &pb.Call{Id: 1, Method: "test/Echo"}}})
	h.closed(CloseProtocol)
}

func TestSocketRefusesAnOldLayer(t *testing.T) {
	h := newLinkHarness(t, &fakeCalls{})
	h.up(&pb.Up{Frame: &pb.Up_Hello{Hello: &pb.SocketHello{Token: "tok"}}})
	h.closed(CloseOldLayer)
}

func TestRevokedSocketCloses(t *testing.T) {
	h := newLinkHarness(t, &fakeCalls{})
	h.hello("tok", 0)
	h.attach("tok")
	h.next()
	h.engine.Send(h.link, Revoked{})
	h.closed(CloseRevoked)
}
