// Command webhookbot is a GoChat bot that takes its updates by webhook and
// answers over the HTTP Bot API: /ping answers pong.
//
//	GOCHAT_URL=https://chat.example.com GOCHAT_BOT_TOKEN=b_... \
//	GOCHAT_WEBHOOK_SECRET=... go run ./examples/webhookbot -addr :8443
//
// The server delivers only to https URLs on public addresses: put it behind
// a TLS-terminating proxy, or give it -cert and -key. setWebhook, with the
// same secret, points the bot at it; docs/bots.md shows how.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/pkg/gochat"
)

// secretHeader carries the webhook's secret on each delivery.
const secretHeader = "X-GoChat-Bot-Secret" //nolint:gosec // a header name, not a secret

var (
	// Updates are protobuf's JSON mapping; fields newer than this program
	// are skipped.
	jsonIn  = protojson.UnmarshalOptions{DiscardUnknown: true}
	jsonOut = protojson.MarshalOptions{UseProtoNames: true}
)

func main() {
	addr := flag.String("addr", ":8443", "the address to listen on")
	cert := flag.String("cert", "", "a TLS certificate, to serve https itself")
	key := flag.String("key", "", "the certificate's key")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	b := &bot{
		api:    strings.TrimRight(os.Getenv("GOCHAT_URL"), "/") + "/api/v2/bot/",
		token:  os.Getenv("GOCHAT_BOT_TOKEN"),
		secret: os.Getenv("GOCHAT_WEBHOOK_SECRET"),
		http:   &http.Client{Timeout: 10 * time.Second},
	}
	if err := run(ctx, b, *addr, *cert, *key); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, b *bot, addr, cert, key string) error {
	if b.api == "/api/v2/bot/" || b.token == "" || b.secret == "" {
		return errors.New("set GOCHAT_URL, GOCHAT_BOT_TOKEN and GOCHAT_WEBHOOK_SECRET")
	}
	me := &pb.MeResponse{}
	if err := b.call(ctx, "getMe", &pb.MeRequest{}, me); err != nil {
		return err
	}
	b.me = me.User
	log.Printf("signed in as @%s, listening on %s", b.me.GetUsername(), addr)

	srv := &http.Server{Addr: addr, Handler: b, ReadHeaderTimeout: 10 * time.Second}
	// Shutting down lets the delivery in progress finish.
	down := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(down)
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	})
	var err error
	if cert != "" {
		err = srv.ListenAndServeTLS(cert, key)
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		<-down
		return nil
	}
	stop()
	return err
}

type bot struct {
	api, token, secret string
	http               *http.Client
	me                 *pb.User
}

// ServeHTTP takes one delivery. The next update waits for its answer, and
// one unanswered in 10 seconds comes again: answer quickly.
func (b *bot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(secretHeader)), []byte(b.secret)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Any answer but 2xx has the server send the update again, holding
	// back those after it: one the bot cannot handle is still taken.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ev := &pb.Event{}
	if err := jsonIn.Unmarshal(body, ev); err != nil {
		log.Printf("an update that does not parse: %v", err)
	} else if err := b.handle(ctx, ev); err != nil {
		log.Printf("update %d: %v", ev.GetPts(), err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *bot) handle(ctx context.Context, ev *pb.Event) error {
	m := ev.GetMessagePosted().GetMessage()
	if m == nil || m.GetAuthor().GetId() == b.me.GetId() {
		return nil
	}
	if name, _, ok := gochat.Command(m.Content, b.me.GetUsername()); !ok || name != "ping" {
		return nil
	}
	return b.call(ctx, "sendMessage", &pb.PostMessageRequest{ChannelId: m.ChannelId, RootId: m.RootId, Content: "pong"}, &pb.PostMessageResponse{})
}

// call makes one call of the HTTP Bot API.
func (b *bot) call(ctx context.Context, method string, in, out proto.Message) error {
	body, err := jsonOut.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.api+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	var answer struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&answer); err != nil {
		return fmt.Errorf("%s: %s", method, res.Status)
	}
	if !answer.OK {
		return fmt.Errorf("%s: %s (%d)", method, answer.Description, res.StatusCode)
	}
	return jsonIn.Unmarshal(answer.Result, out)
}
