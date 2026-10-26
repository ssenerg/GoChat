package bots

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/callbacks"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/updates"
)

func TestOnlyPublicAddressesTakeWebhooks(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "2002:a00:1::1",
		"224.0.0.1", "255.255.255.255", "198.18.0.1",
	} {
		if Public(netip.MustParseAddr(raw)) {
			t.Errorf("%s passed as public", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "2002:5db8:d822::1"} {
		if !Public(netip.MustParseAddr(raw)) {
			t.Errorf("%s did not pass as public", raw)
		}
	}
}

func TestWebhookURLs(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/hook", "https://user:pw@example.com/", "https://127.0.0.1/hook", "https://[::1]/",
		"https:///nohost", "ftp://example.com", "https://example.com/#frag", "javascript:alert(1)",
	} {
		if checkWebhook(raw, false) == nil {
			t.Errorf("%s was taken", raw)
		}
	}
	if err := checkWebhook("https://bots.example.com/hook?k=v", false); err != nil {
		t.Errorf("a public https webhook: %v", err)
	}
	if err := checkWebhook("http://127.0.0.1:9999/hook", true); err != nil {
		t.Errorf("a local webhook in development: %v", err)
	}
}

func TestClientRefusesWhatResolvesInside(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	// "localhost" passes the URL check, which sees a name; the dialer sees
	// the address it resolves to.
	url := "http://localhost:" + srv.URL[len("http://127.0.0.1:"):]
	if _, err := Client(time.Second, false).Get(url); err == nil {
		t.Fatal("the client reached a loopback address")
	}
	res, err := Client(time.Second, true).Get(url)
	if err != nil {
		t.Fatalf("with private webhooks allowed: %v", err)
	}
	_ = res.Body.Close()
}

func TestClientFollowsNoRedirect(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer target.Close()
	srv := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer srv.Close()
	res, err := Client(time.Second, true).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if hit || res.StatusCode != http.StatusFound {
		t.Fatalf("followed a redirect: %d", res.StatusCode)
	}
}

func TestAnswersGoToThePresserOnly(t *testing.T) {
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan hub.Deliver, 4)
	h := engine.SpawnFunc(func(c *actor.Context) {
		if d, ok := c.Message().(hub.Deliver); ok {
			got <- d
		}
	}, "hub")
	reg := engine.Spawn(callbacks.New(h), "callbacks")
	bot, presser := ids.New(), ids.New()
	engine.Send(reg, callbacks.Pending{QueryID: "q", Presser: presser, Bot: bot, Expires: time.Now().Add(time.Minute)})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := actors.Ask[callbacks.Done](ctx, engine, reg, callbacks.Answer{QueryID: "q", Bot: &pb.User{Id: ids.New().String()}}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("another bot answered: %v", err)
	}
	if _, err := actors.Ask[callbacks.Done](ctx, engine, reg, callbacks.Answer{QueryID: "q", Bot: &pb.User{Id: bot.String()}, Text: "Saved"}); err != nil {
		t.Fatal(err)
	}
	d := <-got
	if len(d.UserIDs) != 1 || d.UserIDs[0] != presser.String() || d.Event.GetCallbackAnswer().GetText() != "Saved" || d.Event.Pts != 0 {
		t.Fatalf("delivered %+v", d)
	}
	if _, err := actors.Ask[callbacks.Done](ctx, engine, reg, callbacks.Answer{QueryID: "q", Bot: &pb.User{Id: bot.String()}}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a press answered twice: %v", err)
	}
}

// memRepo keeps one bot, whose account is at pts.
type memRepo struct {
	Repo
	mu  sync.Mutex
	bot *models.Bot
	pts int64
}

func (r *memRepo) AccountPts(context.Context, gocql.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pts, nil
}

func (r *memRepo) SetWebhook(_ context.Context, _ gocql.UUID, url, secret string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bot.WebhookURL, r.bot.WebhookSecret = url, secret
	return nil
}

func (r *memRepo) GetBot(context.Context, gocql.UUID) (*models.Bot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *r.bot
	return &c, nil
}

func (r *memRepo) SetBotCommands(_ context.Context, _ gocql.UUID, b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bot.Commands = b
	return nil
}

func (r *memRepo) SetDelivered(_ context.Context, _ gocql.UUID, pts int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bot.DeliveredPts = pts
	return nil
}

func (r *memRepo) SetWebhookError(_ context.Context, _ gocql.UUID, reason string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bot.WebhookError = reason
	return nil
}

func (r *memRepo) ListWebhooks(context.Context) ([]*models.Bot, error) { return nil, nil }

func (r *memRepo) delivered() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bot.DeliveredPts
}

func TestCommandsAreChecked(t *testing.T) {
	repo := &memRepo{bot: &models.Bot{ID: ids.New()}}
	w := &worker{Deps: &Deps{Logger: zap.NewNop(), Repo: repo}}
	bot := &models.Principal{UserID: repo.bot.ID, Bot: true}
	ctx := context.Background()
	if _, err := w.setCommands(ctx, SetCommands{P: bot, Commands: []*pb.BotCommand{{Name: "/Weather", Description: "The weather"}, {Name: "help", Description: "Help"}}}); err != nil {
		t.Fatal(err)
	}
	cmds, err := w.getCommands(ctx, bot)
	if err != nil || len(cmds) != 2 || cmds[0].Name != "weather" {
		t.Fatalf("commands %v, %v", cmds, err)
	}
	for name, bad := range map[string][]*pb.BotCommand{
		"spaces":      {{Name: "two words", Description: "x"}},
		"twice":       {{Name: "a", Description: "x"}, {Name: "a", Description: "y"}},
		"undescribed": {{Name: "a"}},
	} {
		if _, err := w.setCommands(ctx, SetCommands{P: bot, Commands: bad}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := w.setCommands(ctx, SetCommands{P: &models.Principal{UserID: ids.New()}}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Errorf("a person set commands: %v", err)
	}
}

func TestWebhookDeliversInOrderAndRetries(t *testing.T) {
	var (
		mu       sync.Mutex
		bodies   []map[string]any
		failNext = true
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get(SecretHeader) != "s3cret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if failNext {
			failNext = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies = append(bodies, m)
	}))
	defer srv.Close()
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	botID := ids.New()
	repo := &memRepo{bot: &models.Bot{ID: botID, WebhookURL: srv.URL, WebhookSecret: "s3cret"}}
	// A stand-in sequencer with four updates, the first two about messages
	// deleted since.
	upd := engine.SpawnFunc(func(c *actor.Context) {
		if d, ok := c.Message().(updates.GetDifference); ok {
			if d.Pts < 2 {
				actors.Reply(c, &updates.Difference{Pts: 2, More: true})
				return
			}
			var out []*pb.Event
			for pts := d.Pts + 1; pts <= 4; pts++ {
				out = append(out, &pb.Event{Pts: pts, PtsCount: 1, Payload: &pb.Event_ChannelMuted{ChannelMuted: &pb.ChannelMuted{ChannelId: "c"}}})
			}
			actors.Reply(c, &updates.Difference{Updates: out, Pts: max(d.Pts, 4)})
		}
	}, "updates")
	d := &WebhookDeps{
		Deps:    &Deps{Logger: zap.NewNop(), Repo: repo, Engine: engine, Cfg: &Config{OpTimeout: time.Second, WebhookRetryMax: time.Second}},
		Updates: upd, Client: Client(time.Second, true),
	}
	hooks := engine.Spawn(Webhooks(d), "webhooks")
	defer engine.Poison(hooks)
	engine.Send(hooks, Hooked{BotID: botID})
	for deadline := time.Now().Add(5 * time.Second); repo.delivered() != 4; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("delivered up to %d", repo.delivered())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0]["pts"] != "3" || bodies[1]["pts"] != "4" || bodies[0]["channel_muted"] == nil {
		t.Fatalf("bodies %v", bodies)
	}
}

func TestAWebhookStartsFromNow(t *testing.T) {
	repo := &memRepo{bot: &models.Bot{ID: ids.New()}, pts: 40}
	w := &worker{Deps: &Deps{Logger: zap.NewNop(), Repo: repo, Cfg: &Config{AllowPrivateWebhooks: true}}}
	bot := &models.Principal{UserID: repo.bot.ID, Bot: true}
	ctx := context.Background()
	if _, err := w.setWebhook(ctx, SetWebhook{P: bot, URL: "https://example.com/hook"}); err != nil {
		t.Fatal(err)
	}
	if got := repo.delivered(); got != 40 {
		t.Fatalf("a new webhook starts after %d, want 40", got)
	}
	// Changing its address keeps its place.
	repo.pts = 45
	if _, err := w.setWebhook(ctx, SetWebhook{P: bot, URL: "https://example.com/other"}); err != nil || repo.delivered() != 40 {
		t.Fatalf("moved webhook: delivered %d, %v", repo.delivered(), err)
	}
	// No one confirms past the latest update.
	if _, err := w.confirm(ctx, Confirm{P: bot, Pts: 1000}); err != nil || repo.delivered() != 45 {
		t.Fatalf("confirmed %d, %v", repo.delivered(), err)
	}
}
