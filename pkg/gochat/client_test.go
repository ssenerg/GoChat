package gochat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
)

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		text, name, args string
		ok               bool
	}{
		{"/echo hi there", "echo", "hi there", true},
		{"/Echo@echo_bot  hi", "echo", "hi", true},
		{"/echo@other_bot hi", "", "", false},
		{"/", "", "", false},
		{"echo", "", "", false},
	} {
		name, args, ok := Command(tc.text, "echo_bot")
		if name != tc.name || args != tc.args || ok != tc.ok {
			t.Errorf("%q: %q %q %v", tc.text, name, args, ok)
		}
	}
}

func TestSocketURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://chat.example.com":                "wss://chat.example.com/api/v2/socket",
		"http://localhost:8080/":                  "ws://localhost:8080/api/v2/socket",
		"wss://chat.example.com/gochat/api/v2/ws": "wss://chat.example.com/gochat/api/v2/ws",
	} {
		if got, err := socketURL(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	if _, err := socketURL("ftp://chat.example.com"); err == nil {
		t.Error("an ftp URL was taken")
	}
}

// fakeServer runs each connection's script in turn.
func fakeServer(t *testing.T, scripts ...func(*fakeConn)) string {
	t.Helper()
	next := make(chan func(*fakeConn), len(scripts))
	for _, s := range scripts {
		next <- s
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"gochat.v2"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		select {
		case script := <-next:
			script(&fakeConn{t: t, conn: conn})
			// Held open until the client goes.
			for {
				if _, _, err := conn.Read(context.Background()); err != nil {
					return
				}
			}
		default:
			_ = conn.Close(4000, "no more scripts")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type fakeConn struct {
	t    *testing.T
	conn *websocket.Conn
}

func (f *fakeConn) read() *pb.Up {
	_, data, err := f.conn.Read(context.Background())
	if err != nil {
		f.t.Errorf("read: %v", err)
		return &pb.Up{}
	}
	var up pb.Up
	_ = proto.Unmarshal(data, &up)
	return &up
}

func (f *fakeConn) send(d *pb.Down) {
	b, _ := proto.Marshal(d)
	_ = f.conn.Write(context.Background(), websocket.MessageBinary, b)
}

func welcome(pts int64, reload bool) *pb.Down {
	return &pb.Down{Frame: &pb.Down_Welcome{Welcome: &pb.SocketWelcome{User: &pb.User{Id: "bot"}, Pts: pts, Reload: reload}}}
}

func updates(evs ...*pb.Event) *pb.Down {
	return &pb.Down{Frame: &pb.Down_Updates{Updates: &pb.Updates{Updates: evs}}}
}

func account(pts int64) *pb.Event { return &pb.Event{Pts: pts, PtsCount: 1} }

func next(t *testing.T, c *Client) Update {
	t.Helper()
	select {
	case u, ok := <-c.Updates():
		if !ok {
			t.Fatal("the updates ended")
		}
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("no update")
	}
	return Update{}
}

func TestResumesAfterTheLastUpdate(t *testing.T) {
	url := fakeServer(t,
		func(f *fakeConn) {
			if h := f.read().GetHello(); h.GetToken() != "b_token" || h.GetPts() != 0 || h.GetLayer() != Layer {
				t.Errorf("first hello: %v", h)
			}
			f.send(welcome(10, false))
			f.send(updates(account(11), &pb.Event{Pts: 5, PtsCount: 1, PtsChannelId: "c"}))
			f.send(&pb.Down{Frame: &pb.Down_Ephemeral{Ephemeral: &pb.Event{Payload: &pb.Event_Typing{Typing: &pb.Typing{}}}}})
			call := f.read().GetCall()
			if call.GetMethod() != "gochat.v1.UpdatesService/GetState" {
				t.Errorf("call: %v", call)
			}
			res, _ := proto.Marshal(&pb.GetStateResponse{Pts: 11})
			f.send(&pb.Down{Frame: &pb.Down_Result{Result: &pb.Result{Id: call.GetId(), Outcome: &pb.Result_Payload{Payload: res}}}})
			f.send(updates(account(12)))
			_ = f.conn.Close(4000, "bye")
		},
		func(f *fakeConn) {
			if h := f.read().GetHello(); h.GetPts() != 12 {
				t.Errorf("second hello: %v", h)
			}
			f.send(welcome(12, false))
			f.send(updates(account(12), account(13)))
		},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, Options{URL: url, Token: "b_token"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if c.Me().GetId() != "bot" {
		t.Fatalf("me: %v", c.Me())
	}
	var state pb.GetStateResponse
	if err := c.Call(ctx, "gochat.v1.UpdatesService/GetState", &pb.GetStateRequest{}, &state); err != nil || state.Pts != 11 {
		t.Fatalf("call: %v %v", &state, err)
	}
	if u := next(t, c); u.Event.Pts != 11 {
		t.Fatalf("first: %v", u)
	}
	if u := next(t, c); u.Event.PtsChannelId != "c" {
		t.Fatalf("channel update: %v", u)
	}
	if u := next(t, c); !u.Ephemeral {
		t.Fatalf("typing: %v", u)
	}
	if u := next(t, c); u.Event.Pts != 12 {
		t.Fatalf("before the drop: %v", u)
	}
	if u := next(t, c); u.Event.Pts != 13 {
		t.Fatalf("after it, 12 again or not 13: %v", u)
	}
	if c.Pts() != 13 {
		t.Fatalf("pts %d", c.Pts())
	}
}

func TestReloadsWhenTheServerCannotReplay(t *testing.T) {
	url := fakeServer(t, func(f *fakeConn) {
		f.read()
		f.send(welcome(50, true))
		f.send(updates(account(51)))
	})
	c, err := Dial(context.Background(), Options{URL: url, Token: "b_token", Pts: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if u := next(t, c); !u.Reload {
		t.Fatalf("first: %v", u)
	}
	if u := next(t, c); u.Event.Pts != 51 {
		t.Fatalf("then: %v", u)
	}
}

func TestAnInvalidTokenStopsTheClient(t *testing.T) {
	url := fakeServer(t, func(f *fakeConn) {
		f.read()
		_ = f.conn.Close(4001, "unauthenticated")
	})
	_, err := Dial(context.Background(), Options{URL: url, Token: "b_wrong"})
	if err == nil || !strings.Contains(err.Error(), "4001") {
		t.Fatalf("dial: %v", err)
	}
}

func TestCloseEndsUpdatesAndCalls(t *testing.T) {
	url := fakeServer(t, func(f *fakeConn) {
		f.read()
		f.send(welcome(1, false))
		f.read()
	})
	c, err := Dial(context.Background(), Options{URL: url, Token: "b_token"})
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() {
		failed <- c.Call(context.Background(), "gochat.v1.UpdatesService/GetState", &pb.GetStateRequest{}, &pb.GetStateResponse{})
	}()
	time.Sleep(50 * time.Millisecond)
	_ = c.Close()
	if err := <-failed; !errors.Is(err, ErrClosed) {
		t.Fatalf("call: %v", err)
	}
	if _, ok := <-c.Updates(); ok {
		t.Fatal("the updates went on")
	}
}

func TestACallTheConnectionDropsUnderIsNotMadeAgain(t *testing.T) {
	calls := make(chan string, 4)
	url := fakeServer(t,
		func(f *fakeConn) {
			f.read()
			f.send(welcome(1, false))
			calls <- f.read().GetCall().GetMethod()
			_ = f.conn.Close(4000, "bye")
		},
		func(f *fakeConn) {
			f.read()
			f.send(welcome(1, false))
			// The client closes this one without a call.
			if _, data, err := f.conn.Read(context.Background()); err == nil {
				var up pb.Up
				if proto.Unmarshal(data, &up) == nil && up.GetCall() != nil {
					calls <- up.GetCall().GetMethod()
				}
			}
		},
	)
	c, err := Dial(context.Background(), Options{URL: url, Token: "b_token"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	err = c.Call(context.Background(), "gochat.v1.MessageService/PostMessage", &pb.PostMessageRequest{}, &pb.PostMessageResponse{})
	if !errors.Is(err, ErrLost) {
		t.Fatalf("call: %v", err)
	}
	select {
	case m := <-calls:
		if m != "gochat.v1.MessageService/PostMessage" {
			t.Fatalf("first call %q", m)
		}
	default:
		t.Fatal("the server never got the call")
	}
	select {
	case m := <-calls:
		t.Fatalf("the call was made again: %q", m)
	case <-time.After(time.Second):
	}
}
