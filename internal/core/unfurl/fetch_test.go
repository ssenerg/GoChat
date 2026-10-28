package unfurl

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var loopback = netip.MustParseAddr("127.0.0.1")

// dns answers names from a table, each lookup taking the next answer and
// the last repeating.
type dns struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	asked   map[string]int
}

func newDNS() *dns {
	return &dns{answers: map[string][][]netip.Addr{}, asked: map[string]int{}}
}

func (d *dns) set(host string, answers ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range answers {
		var list []netip.Addr
		for s := range strings.SplitSeq(a, ",") {
			list = append(list, netip.MustParseAddr(s))
		}
		d.answers[host] = append(d.answers[host], list)
	}
}

func (d *dns) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := d.answers[host]
	if len(list) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	n := d.asked[host]
	d.asked[host]++
	return append([]netip.Addr(nil), list[min(n, len(list)-1)]...), nil
}

func (d *dns) lookups(host string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked[host]
}

// site serves h on 127.0.0.1, counting its requests.
type site struct {
	*httptest.Server
	port int
	hits atomic.Int32
}

func serve(t *testing.T, h http.HandlerFunc) *site {
	t.Helper()
	s := &site{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(s.Close)
	s.port = s.Listener.Addr().(*net.TCPAddr).Port
	return s
}

func (s *site) at(host, path string) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(s.port)) + path
}

// testFetcher reaches only 127.0.0.1, standing in for the internet, on the
// test site's port and 80.
func testFetcher(d *dns, s *site) *Fetcher {
	return &Fetcher{
		Timeout: 2 * time.Second, MaxRedirects: 3, Ports: []int{80, s.port}, UserAgent: "GoChat-test",
		Permit: func(a netip.Addr) bool { return a == loopback }, Resolver: d,
	}
}

func page(url string) Request {
	return Request{URL: url, Accept: "text/html", Takes: IsHTML, MaxBytes: 1 << 20, Truncate: true}
}

func html200(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}
}

func TestFetches(t *testing.T) {
	d := newDNS()
	d.set("site.test", "127.0.0.1")
	s := serve(t, html200("<title>Hi</title>"))
	res, err := testFetcher(d, s).Fetch(context.Background(), page(s.at("site.test", "/a#frag")))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "<title>Hi</title>" || res.MediaType != "text/html" || res.URL.Fragment != "" || res.Truncated {
		t.Fatalf("got %+v", res)
	}
}

func TestRefusesAddressesThatAreNotPublic(t *testing.T) {
	s := serve(t, html200("secret"))
	d := newDNS()
	d.set("private.test", "10.0.0.1")
	d.set("metadata.test", "169.254.169.254")
	d.set("loop6.test", "::1")
	d.set("mapped.test", "::ffff:127.0.0.1")
	// A name that points outward and inward at once is refused whole.
	d.set("both.test", "93.184.216.34,192.168.1.1")
	f := &Fetcher{Timeout: time.Second, MaxRedirects: 3, Ports: []int{80, s.port}, UserAgent: "GoChat-test", Resolver: d}
	for _, raw := range []string{
		s.at("127.0.0.1", "/"), s.at("localhost.test", "/"), s.at("::1", "/"), "http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/", "http://[::ffff:7f00:1]/", s.at("private.test", "/"), s.at("metadata.test", "/"),
		s.at("loop6.test", "/"), s.at("mapped.test", "/"), s.at("both.test", "/"),
	} {
		_, err := f.Fetch(context.Background(), page(raw))
		if raw == s.at("localhost.test", "/") {
			// Unknown to the resolver: it fails to resolve.
			if err == nil {
				t.Errorf("%s fetched", raw)
			}
			continue
		}
		if !errors.Is(err, ErrAddress) {
			t.Errorf("%s: %v, want ErrAddress", raw, err)
		}
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("the server was reached %d times", n)
	}
}

func TestRefusesURLs(t *testing.T) {
	s := serve(t, html200("x"))
	d := newDNS()
	d.set("site.test", "127.0.0.1")
	f := testFetcher(d, s)
	for _, raw := range []string{
		"ftp://site.test/", "javascript:alert(1)", "file:///etc/passwd", "http://user:pw@site.test/", "http:///nohost",
		"http://site.test:1/", "https://site.test:22/", "gopher://site.test:70/", "http://site.test:0/",
		"http://[fe80::1%25en0]/", "http://" + strings.Repeat("a", MaxURL) + ".test/",
	} {
		if _, err := f.Fetch(context.Background(), page(raw)); !errors.Is(err, ErrURL) {
			t.Errorf("%s: %v, want ErrURL", raw, err)
		}
	}
	host := func(h string) bool { return h != "site.test" }
	req := page(s.at("SITE.test.", "/"))
	req.Host = host
	if _, err := f.Fetch(context.Background(), req); !errors.Is(err, ErrHost) {
		t.Errorf("a denied host: %v", err)
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("the server was reached %d times", n)
	}
}

func TestRedirects(t *testing.T) {
	d := newDNS()
	d.set("site.test", "127.0.0.1")
	d.set("internal.test", "10.0.0.1")
	d.set("denied.test", "127.0.0.1")
	var s *site
	s = serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/final":
			html200("<title>Final</title>")(w, r)
		case "/relative":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusMovedPermanently)
		default:
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
		}
	})
	f := testFetcher(d, s)
	res, err := f.Fetch(context.Background(), page(s.at("site.test", "/relative")))
	if err != nil || string(res.Body) != "<title>Final</title>" || res.URL.Path != "/final" {
		t.Fatalf("a relative redirect: %v %+v", err, res)
	}
	if _, err := f.Fetch(context.Background(), page(s.at("site.test", "/loop"))); !errors.Is(err, ErrRedirects) {
		t.Fatalf("a loop: %v", err)
	}
	hop := func(to string) error {
		req := page(s.at("site.test", "/go?to="+url.QueryEscape(to)))
		req.Host = func(h string) bool { return h != "denied.test" }
		_, err := f.Fetch(context.Background(), req)
		return err
	}
	for to, want := range map[string]error{
		"http://169.254.169.254/latest/meta-data/": ErrAddress,
		s.at("::1", "/final"):                      ErrAddress,
		s.at("10.1.2.3", "/final"):                 ErrAddress,
		s.at("internal.test", "/final"):            ErrAddress,
		"file:///etc/passwd":                       ErrURL,
		"http://site.test:6379/":                   ErrURL,
		s.at("denied.test", "/final"):              ErrHost,
	} {
		if err := hop(to); !errors.Is(err, want) {
			t.Errorf("redirect to %s: %v, want %v", to, err, want)
		}
	}
	if err := hop(s.at("site.test", "/final")); err != nil {
		t.Errorf("a redirect to an allowed address: %v", err)
	}
}

// TestPinsTheCheckedAddress: a name that answers a public address to the
// check and a private one after is connected to at the address checked,
// and a later hop that resolves it inward is refused.
func TestPinsTheCheckedAddress(t *testing.T) {
	d := newDNS()
	d.set("rebind.test", "127.0.0.1", "10.0.0.1")
	d.set("again.test", "127.0.0.1", "10.0.0.1")
	var s *site
	s = serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/again" {
			http.Redirect(w, r, s.at("again.test", "/"), http.StatusFound)
			return
		}
		html200("<title>Pinned</title>")(w, r)
	})
	f := testFetcher(d, s)
	res, err := f.Fetch(context.Background(), page(s.at("rebind.test", "/")))
	if err != nil || string(res.Body) != "<title>Pinned</title>" {
		t.Fatalf("pinned fetch: %v", err)
	}
	if n := d.lookups("rebind.test"); n != 1 {
		t.Fatalf("looked up %d times for one hop", n)
	}
	if _, err := f.Fetch(context.Background(), page(s.at("again.test", "/again"))); !errors.Is(err, ErrAddress) {
		t.Fatalf("the hop after the rebind: %v", err)
	}
	if n := s.hits.Load(); n != 2 {
		t.Fatalf("%d requests reached the site, want 2", n)
	}
}

func TestLimitsSize(t *testing.T) {
	d := newDNS()
	d.set("site.test", "127.0.0.1")
	big := bytes.Repeat([]byte("a"), 3<<20)
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	_, _ = zw.Write(make([]byte, 64<<20))
	_ = zw.Close()
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/declared":
			w.Header().Set("Content-Length", strconv.Itoa(len(big)))
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(big)
		case "/chunked":
			w.Header().Set("Content-Type", "image/png")
			for i := 0; i < len(big); i += 64 << 10 {
				_, _ = w.Write(big[i : i+64<<10])
				w.(http.Flusher).Flush()
			}
		case "/bomb":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(bomb.Bytes())
		}
	})
	f := testFetcher(d, s)
	image := func(path string) Request {
		return Request{URL: s.at("site.test", path), Takes: IsImage, MaxBytes: 1 << 20}
	}
	for _, path := range []string{"/declared", "/chunked"} {
		if _, err := f.Fetch(context.Background(), image(path)); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v, want ErrTooLarge", path, err)
		}
	}
	res, err := f.Fetch(context.Background(), page(s.at("site.test", "/bomb")))
	if err != nil || len(res.Body) != 1<<20 || !res.Truncated {
		t.Fatalf("a page that unpacks past the limit is cut: %v", err)
	}
	req := page(s.at("site.test", "/bomb"))
	req.Truncate = false
	if _, err := f.Fetch(context.Background(), req); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an untruncated bomb: %v", err)
	}
}

func TestLimitsTime(t *testing.T) {
	d := newDNS()
	d.set("site.test", "127.0.0.1")
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/headers":
			// Answers nothing until the client gives up.
			<-r.Context().Done()
		case "/drip":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			for range 100 {
				if _, err := w.Write([]byte("<p>")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-time.After(50 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
		}
	})
	f := testFetcher(d, s)
	f.Timeout = 300 * time.Millisecond
	for _, path := range []string{"/headers", "/drip"} {
		start := time.Now()
		_, err := f.Fetch(context.Background(), page(s.at("site.test", path)))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: %v, want a deadline", path, err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s took %v", path, took)
		}
	}
}

func TestStatusAndType(t *testing.T) {
	d := newDNS()
	d.set("site.test", "127.0.0.1")
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/pdf":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF"))
		}
	})
	f := testFetcher(d, s)
	if _, err := f.Fetch(context.Background(), page(s.at("site.test", "/missing"))); !errors.Is(err, ErrStatus) {
		t.Errorf("404: %v", err)
	}
	if _, err := f.Fetch(context.Background(), page(s.at("site.test", "/pdf"))); !errors.Is(err, ErrType) {
		t.Errorf("pdf: %v", err)
	}
}

// TestSendsNoCredentials: cookies a site sets are not sent back, and no
// credentials go with any hop.
func TestSendsNoCredentials(t *testing.T) {
	d := newDNS()
	d.set("site.test", "127.0.0.1")
	var mu sync.Mutex
	var seen []http.Header
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		if r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "x"})
			http.Redirect(w, r, "/next", http.StatusFound)
			return
		}
		html200("ok")(w, r)
	})
	if _, err := testFetcher(d, s).Fetch(context.Background(), page(s.at("site.test", "/"))); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("%d requests", len(seen))
	}
	for _, h := range seen {
		if h.Get("Cookie") != "" || h.Get("Authorization") != "" || h.Get("User-Agent") != "GoChat-test" {
			t.Fatalf("sent %v", h)
		}
	}
}

func TestHosts(t *testing.T) {
	h := Hosts{Deny: []string{"Evil.example"}}
	if h.Allows("evil.example") || h.Allows("www.evil.example.") || !h.Allows("notevil.example") || !h.Denies("cdn.evil.example") {
		t.Fatal("deny list")
	}
	h = Hosts{Allow: []string{"docs.example"}, Deny: []string{"private.docs.example"}}
	if !h.Allows("docs.example") || !h.Allows("api.docs.example") || h.Allows("example") || h.Allows("private.docs.example") {
		t.Fatal("allow list")
	}
	if h.Denies("cdn.other") {
		t.Fatal("pictures follow the deny list alone")
	}
}
