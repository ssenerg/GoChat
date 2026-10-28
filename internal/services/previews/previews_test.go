package previews

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/unfurl"
	"github.com/ssenerg/gochat/internal/services/channels"
)

type cache struct {
	mu   sync.Mutex
	rows map[string][]byte
	ttls map[string]time.Duration
}

func (c *cache) GetLinkPreview(_ context.Context, key []byte) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.rows[string(key)]
	return v, ok, nil
}

func (c *cache) PutLinkPreview(_ context.Context, key, preview []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows[string(key)], c.ttls[string(key)] = preview, ttl
	return nil
}

type store struct {
	mu   sync.Mutex
	objs map[string]string
}

func (s *store) Put(_ context.Context, key string, r io.Reader, ctype string) (int64, error) {
	b, _ := io.ReadAll(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = ctype
	return int64(len(b)), nil
}

type resolver struct{}

func (resolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func picture(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), 100, 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// harness serves pages on 127.0.0.1 as if public, and collects what the
// service hands the channels.
type harness struct {
	s     *Service
	cache *cache
	store *store
	got   chan channels.Previewed
	hits  atomic.Int32
	base  string
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{cache: &cache{rows: map[string][]byte{}, ttls: map[string]time.Duration{}}, store: &store{objs: map[string]string{}}, got: make(chan channels.Previewed, 8)}
	big := picture(t, 1200, 630)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		switch r.URL.Path {
		case "/card.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(big)
		case "/pixel.png":
			_, _ = w.Write(picture(t, 1, 1))
		case "/plain":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<body>no head</body>`))
		case "/pixel":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<title>Pixel</title><meta property="og:image" content="/pixel.png">`))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<head><meta property="og:title" content="The page"><meta property="og:image" content="/card.png"></head>`))
		}
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	h.base = "http://site.test:" + strconv.Itoa(port)
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	sink := engine.SpawnFunc(func(c *actor.Context) {
		if p, ok := c.Message().(channels.Previewed); ok {
			h.got <- p
		}
	}, "channels")
	cfg.Workers, cfg.Timeout, cfg.CacheTTL, cfg.FailureTTL, cfg.UserAgent = 2, 2*time.Second, time.Hour, time.Minute, "test"
	cfg.MaxPageSize, cfg.MaxImageSize, cfg.Ports = 1<<20, 5<<20, []int{port}
	if cfg.Queue == 0 {
		cfg.Queue = 16
	}
	h.s = Start(&Deps{
		Logger: zap.NewNop(), Cfg: &cfg, Repo: h.cache, Storage: h.store, Engine: engine, Channels: sink,
		Fetcher: &unfurl.Fetcher{
			Timeout: cfg.Timeout, MaxRedirects: 2, Ports: cfg.Ports, UserAgent: cfg.UserAgent, Resolver: resolver{},
			Permit: func(a netip.Addr) bool { return a == netip.MustParseAddr("127.0.0.1") },
		},
	})
	return h
}

func (h *harness) ask(path string) channels.Unfurl {
	job := channels.Unfurl{ChannelID: ids.New(), MessageID: ids.NewTime(), URL: h.base + path}
	h.s.Preview(job)
	return job
}

func (h *harness) next(t *testing.T) (channels.Previewed, bool) {
	t.Helper()
	select {
	case p := <-h.got:
		return p, true
	case <-time.After(500 * time.Millisecond):
		return channels.Previewed{}, false
	}
}

func TestMakesAndCachesPreviews(t *testing.T) {
	h := newHarness(t, Config{})
	job := h.ask("/post")
	p, ok := h.next(t)
	if !ok || p.Unfurl != job {
		t.Fatalf("got %+v", p)
	}
	lp := p.Preview
	if lp.Url != job.URL || lp.Title != "The page" || lp.SiteName != "site.test" || lp.ImageWidth != 640 || lp.ImageHeight != 336 {
		t.Fatalf("preview %v", lp)
	}
	key, ok := ImageKey(lp.ImageId)
	if !ok || h.store.objs[key] != "image/jpeg" {
		t.Fatalf("picture %q stored as %v", lp.ImageId, h.store.objs)
	}
	if n := h.hits.Load(); n != 2 {
		t.Fatalf("%d fetches for a page and its picture", n)
	}
	// Another message with the link takes the cached preview.
	h.ask("/post")
	if p, ok := h.next(t); !ok || p.Preview.Title != "The page" || h.hits.Load() != 2 {
		t.Fatalf("cached: %v, %d fetches", p.Preview, h.hits.Load())
	}
}

func TestPagesWithoutPreviews(t *testing.T) {
	h := newHarness(t, Config{DenyHosts: []string{"denied.test"}})
	h.ask("/plain")
	if _, ok := h.next(t); ok {
		t.Fatal("a page that says nothing has a preview")
	}
	// Known to have none, it is not fetched again for a while.
	h.ask("/plain")
	if _, ok := h.next(t); ok || h.hits.Load() != 1 {
		t.Fatalf("%d fetches", h.hits.Load())
	}
	for _, ttl := range h.cache.ttls {
		if ttl != time.Minute {
			t.Fatalf("cached for %v", ttl)
		}
	}
	h.s.Preview(channels.Unfurl{ChannelID: ids.New(), MessageID: ids.NewTime(), URL: "http://denied.test/"})
	if _, ok := h.next(t); ok {
		t.Fatal("a denied host was previewed")
	}
	// A picture too small to show leaves the preview without one.
	h.ask("/pixel")
	if p, ok := h.next(t); !ok || p.Preview.Title != "Pixel" || p.Preview.ImageId != "" {
		t.Fatalf("pixel: %v", p.Preview)
	}
}

func TestQueueIsBounded(t *testing.T) {
	h := newHarness(t, Config{Queue: 1})
	h.s.pending.Store(1)
	h.ask("/post")
	if _, ok := h.next(t); ok || h.hits.Load() != 0 {
		t.Fatal("a link past the queue was previewed")
	}
}

func TestConfig(t *testing.T) {
	good := func() Config {
		return Config{
			Enabled: true, Workers: 4, Queue: 10, Timeout: time.Second, MaxRedirects: 3, MaxPageSize: 1 << 20, MaxImageSize: 1 << 20,
			Ports: []int{80, 443}, CacheTTL: time.Hour, FailureTTL: time.Minute, UserAgent: "x",
			DenyHosts: []string{"Bücher.example."}, AllowHosts: []string{},
		}
	}
	c := good()
	if err := c.Validate(); err != nil || c.DenyHosts[0] != "xn--bcher-kva.example" {
		t.Fatalf("%v %v", err, c.DenyHosts)
	}
	for _, bad := range []func(*Config){
		func(c *Config) { c.Workers = 0 },
		func(c *Config) { c.Ports = nil },
		func(c *Config) { c.Ports = []int{70000} },
		func(c *Config) { c.MaxImageSize = 100 << 20 },
		func(c *Config) { c.DenyHosts = []string{"https://x.example/"} },
		func(c *Config) { c.UserAgent = " " },
	} {
		c := good()
		bad(&c)
		if c.Validate() == nil {
			t.Errorf("took %+v", c)
		}
	}
}
