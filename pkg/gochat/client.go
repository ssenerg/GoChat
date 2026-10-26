// Package gochat is a client of the GoChat protocol, for bots and other
// programs: one socket for calls and updates, which it reconnects and
// resumes on its own. docs/protocol.md specifies the protocol.
//
//	c, err := gochat.Dial(ctx, gochat.Options{URL: "wss://chat.example.com", Token: token})
//	for u := range c.Updates() {
//		if m := u.Event.GetMessagePosted().GetMessage(); m != nil { ... }
//	}
package gochat

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
)

// Layer is the protocol layer this client speaks.
const Layer = 1

const (
	writeTimeout = 10 * time.Second
	pingEvery    = 30 * time.Second
)

// Options says where to connect and as whom.
type Options struct {
	// URL is the server, such as https://chat.example.com, or its socket,
	// wss://chat.example.com/api/v2/socket.
	URL string
	// Token is a bot's token or a person's session token.
	Token string
	// Client names the program in the session list, such as "weatherbot/1.2".
	Client string
	// Pts is the last account update the program applied, to resume after;
	// 0 starts from now.
	Pts int64
	// Header is sent with the socket's handshake.
	Header http.Header
}

// Update is one update: numbered, or ephemeral, such as typing. Reload is
// set when updates were lost: the program loads what it shows again.
type Update struct {
	Event     *pb.Event
	Ephemeral bool
	Reload    bool

	pts int64
}

// Error is a call's failure, as the server tells it.
type Error struct {
	Code    uint32
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("gochat: %s (code %d)", e.Message, e.Code) }

// ErrClosed is returned by calls on a closed client.
var ErrClosed = errors.New("gochat: client closed")

// ErrLost fails a call whose connection dropped before its result came:
// the server may have made it, so the client does not make it again.
var ErrLost = errors.New("gochat: the connection dropped before the call's result")

// errDown fails a call the client could not send, which it sends again
// once reconnected.
var errDown = errors.New("gochat: connection lost")

// Client is a connection to a GoChat server that reconnects until closed.
type Client struct {
	opts    Options
	socket  string
	updates chan Update
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu      sync.Mutex
	conn    *websocket.Conn
	ready   chan struct{}
	me      *pb.User
	seen    int64 // the last account update received
	pts     int64 // the last account update handed out
	nextID  uint64
	pending map[uint64]chan *pb.Result
	queue   []Update
	wake    chan struct{}
	err     error
}

// Dial connects and waits for the server's welcome; after that the client
// reconnects by itself, resuming after the last update it handed out.
func Dial(ctx context.Context, opts Options) (*Client, error) {
	socket, err := socketURL(opts.URL)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		opts: opts, socket: socket, updates: make(chan Update), ctx: cctx, cancel: cancel,
		ready: make(chan struct{}), seen: opts.Pts, pts: opts.Pts, pending: make(map[uint64]chan *pb.Result),
		wake: make(chan struct{}, 1),
	}
	first := make(chan error, 1)
	c.wg.Add(2)
	go c.run(first)
	go c.pump()
	select {
	case err := <-first:
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
}

func socketURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("gochat: invalid URL %q", raw)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("gochat: invalid URL %q", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/api/v2/socket"
	}
	return u.String(), nil
}

// Updates is where the client hands out updates, in order. It is closed
// when the client stops.
func (c *Client) Updates() <-chan Update { return c.updates }

// Me is the caller.
func (c *Client) Me() *pb.User {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.me
}

// Pts is the number of the last account update handed out: a program that
// stores it resumes from it with Options.Pts.
func (c *Client) Pts() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pts
}

// Close closes the connection for good.
func (c *Client) Close() error {
	c.cancel()
	c.wg.Wait()
	return nil
}

// Call makes method, such as "gochat.v1.MessageService/PostMessage",
// waiting for the connection when it is down. A call the connection drops
// under fails with ErrLost.
func (c *Client) Call(ctx context.Context, method string, in, out proto.Message) error {
	payload, err := proto.Marshal(in)
	if err != nil {
		return err
	}
	for {
		res, err := c.call(ctx, method, payload)
		if errors.Is(err, errDown) {
			continue
		}
		if err != nil {
			return err
		}
		if e := res.GetError(); e != nil {
			return &Error{Code: e.Code, Message: e.Message}
		}
		return proto.Unmarshal(res.GetPayload(), out)
	}
}

func (c *Client) call(ctx context.Context, method string, payload []byte) (*pb.Result, error) {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.closed()
	}
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, errDown
	}
	c.nextID++
	id := c.nextID
	wait := make(chan *pb.Result, 1)
	c.pending[id] = wait
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	frame, _ := proto.Marshal(&pb.Up{Frame: &pb.Up_Call{Call: &pb.Call{Id: id, Method: method, Payload: payload}}})
	// A write that times out closes the socket: it gets its own deadline
	// rather than the caller's.
	wctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	err := conn.Write(wctx, websocket.MessageBinary, frame)
	cancel()
	if err != nil {
		return nil, errDown
	}
	select {
	case res, ok := <-wait:
		if !ok {
			return nil, ErrLost
		}
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.closed()
	}
}

// closed is why calls fail once the client stopped.
func (c *Client) closed() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrClosed
}

// run keeps a connection up until Close, telling first how the first
// attempt went.
func (c *Client) run(first chan<- error) {
	defer c.wg.Done()
	delay := 500 * time.Millisecond
	for c.ctx.Err() == nil {
		started := time.Now()
		err := c.session(first)
		first = nil
		var fatal *fatalError
		if errors.As(err, &fatal) {
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			c.cancel()
			return
		}
		if c.ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			delay = 500 * time.Millisecond
		}
		jitter := time.Duration(rand.Int64N(int64(delay)/2 + 1)) //nolint:gosec // jitter needs no secrecy
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(delay/2 + jitter):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

// fatalError ends the client: the token is not valid, or the client is too
// old for the server.
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

// session runs one connection. first, when set, hears how the welcome
// went.
func (c *Client) session(first chan<- error) error {
	tell := func(err error) {
		if first != nil {
			first <- err
			first = nil
		}
	}
	dctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dctx, c.socket, &websocket.DialOptions{Subprotocols: []string{"gochat.v2"}, HTTPHeader: c.opts.Header})
	cancel()
	if err != nil {
		tell(err)
		return err
	}
	conn.SetReadLimit(16 << 20)
	defer func() { _ = conn.CloseNow() }()
	go keepAlive(c.ctx, conn)
	c.mu.Lock()
	pts := c.seen
	c.mu.Unlock()
	hello, _ := proto.Marshal(&pb.Up{Frame: &pb.Up_Hello{Hello: &pb.SocketHello{Token: c.opts.Token, Client: c.opts.Client, Layer: Layer, Pts: pts}}})
	if err := conn.Write(c.ctx, websocket.MessageBinary, hello); err != nil {
		tell(err)
		return err
	}
	defer c.down()
	for {
		_, data, err := conn.Read(c.ctx)
		if err != nil {
			switch websocket.CloseStatus(err) {
			case 4001, 4002, 4011:
				err = &fatalError{fmt.Errorf("gochat: the server closed the socket: %w", err)}
			}
			tell(err)
			return err
		}
		var d pb.Down
		if err := proto.Unmarshal(data, &d); err != nil {
			continue
		}
		switch f := d.Frame.(type) {
		case *pb.Down_Welcome:
			c.welcome(conn, f.Welcome)
			tell(nil)
		case *pb.Down_Result:
			c.mu.Lock()
			wait := c.pending[f.Result.Id]
			c.mu.Unlock()
			if wait != nil {
				wait <- f.Result
			}
		case *pb.Down_Updates:
			for _, ev := range f.Updates.Updates {
				c.update(ev)
			}
		case *pb.Down_Ephemeral:
			c.emit(Update{Event: f.Ephemeral, Ephemeral: true})
		}
	}
}

// keepAlive pings the server, closing a connection that stopped answering.
func keepAlive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, writeTimeout)
		err := conn.Ping(pctx)
		cancel()
		if err != nil {
			_ = conn.CloseNow()
			return
		}
	}
}

// welcome starts a connection: the server replays what the client missed
// after it, or says it cannot and the program reloads.
func (c *Client) welcome(conn *websocket.Conn, w *pb.SocketWelcome) {
	c.mu.Lock()
	c.conn, c.me, c.seen = conn, w.User, w.Pts
	select {
	case <-c.ready:
	default:
		close(c.ready)
	}
	c.mu.Unlock()
	if w.Reload {
		c.emit(Update{Reload: true, pts: w.Pts})
	}
}

// update hands out an update once: an account update the client already
// handed out is dropped.
func (c *Client) update(ev *pb.Event) {
	u := Update{Event: ev}
	if ev.PtsChannelId == "" && ev.Pts > 0 {
		c.mu.Lock()
		if ev.Pts <= c.seen {
			c.mu.Unlock()
			return
		}
		c.seen, u.pts = ev.Pts, ev.Pts
		c.mu.Unlock()
	}
	c.emit(u)
}

// emit queues an update, so a program that makes calls while handling one
// never stops the socket from reading their results.
func (c *Client) emit(u Update) {
	c.mu.Lock()
	c.queue = append(c.queue, u)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// pump hands the queued updates out.
func (c *Client) pump() {
	defer c.wg.Done()
	defer close(c.updates)
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.mu.Unlock()
			select {
			case <-c.wake:
				continue
			case <-c.ctx.Done():
				return
			}
		}
		u := c.queue[0]
		c.queue[0] = Update{}
		c.queue = c.queue[1:]
		c.mu.Unlock()
		select {
		case c.updates <- u:
		case <-c.ctx.Done():
			return
		}
		if u.pts > 0 {
			c.mu.Lock()
			c.pts = u.pts
			c.mu.Unlock()
		}
	}
}

// down fails the calls in flight.
func (c *Client) down() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = nil
	select {
	case <-c.ready:
		c.ready = make(chan struct{})
	default:
	}
	for id, wait := range c.pending {
		close(wait)
		delete(c.pending, id)
	}
}

// Err tells why the client stopped by itself, such as a token that is no
// longer valid.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Command reads a command in text: "/weather Tehran" or, addressed to a
// bot, "/weather@weather_bot Tehran". A command addressed to another bot
// than bot is not one.
func Command(text, bot string) (name, args string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	word, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	name, to, addressed := strings.Cut(word, "@")
	if name == "" || (addressed && !strings.EqualFold(to, bot)) {
		return "", "", false
	}
	return strings.ToLower(name), strings.TrimSpace(args), true
}
