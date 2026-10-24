// Package e2e drives a running stack the way browsers do: forms, htmx
// requests and websockets. It runs only when GOCHAT_E2E_URL is set:
//
//	GOCHAT_E2E_URL=http://localhost:8080 GOCHAT_E2E_ADMIN=admin \
//	GOCHAT_E2E_ADMIN_PASSWORD=... GOCHAT_E2E_TEAM=acme go test ./test/e2e/
//
// With GOCHAT_E2E_BOOTSTRAP set it first creates the admin and the team on a
// fresh server, with the server's setup token in GOCHAT_E2E_SETUP_TOKEN.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"image"
	"image/color"
	_ "image/jpeg" // decodes the stored photos
	"image/png"

	"github.com/coder/websocket"
	"github.com/pquerna/otp/totp"

	"github.com/ssenerg/gochat/internal/core/secure"
)

var (
	baseURL   = os.Getenv("GOCHAT_E2E_URL")
	adminName = os.Getenv("GOCHAT_E2E_ADMIN")
	adminPass = os.Getenv("GOCHAT_E2E_ADMIN_PASSWORD")
	teamName  = os.Getenv("GOCHAT_E2E_TEAM")
	setupKey  = os.Getenv("GOCHAT_E2E_SETUP_TOKEN")
)

type client struct {
	t    *testing.T
	http *http.Client
	name string
}

func newClient(t *testing.T, name string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, name: name, http: &http.Client{
		Jar:     jar,
		Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *client) cookie(name string) string {
	u, _ := url.Parse(baseURL)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (c *client) do(method, path string, body io.Reader, contentType string, htmx bool) (*http.Response, string) {
	c.t.Helper()
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", baseURL)
		if tok := c.cookie("gochat_csrf"); tok != "" && htmx {
			req.Header.Set("X-CSRF-Token", tok)
		}
	}
	var buf []byte
	if body != nil {
		buf, _ = io.ReadAll(body)
	}
	// The sign-in endpoints are rate limited per address, which every client
	// here shares: wait for the limiter, which says how long, rather than
	// fail.
	for attempt := 0; ; attempt++ {
		if buf != nil {
			req.Body = io.NopCloser(bytes.NewReader(buf))
		}
		res, err := c.http.Do(req)
		if err != nil {
			c.t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		after := res.Header.Get("Retry-After")
		wait, _ := strconv.Atoi(after)
		if res.StatusCode != http.StatusTooManyRequests || after == "" || attempt == 5 || wait > 15 {
			return res, string(b)
		}
		time.Sleep(time.Duration(wait) * time.Second)
	}
}

func (c *client) get(path string) (*http.Response, string) {
	return c.do(http.MethodGet, path, nil, "", false)
}

// getRange fetches part of path, as a browser's media player does.
func (c *client) getRange(path, ranges string) (*http.Response, string) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Range", ranges)
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func (c *client) form(path string, values url.Values) (*http.Response, string) {
	if c.cookie("gochat_csrf") == "" {
		c.get("/login")
	}
	values.Set("_csrf", c.cookie("gochat_csrf"))
	return c.do(http.MethodPost, path, strings.NewReader(values.Encode()), "application/x-www-form-urlencoded", false)
}

func (c *client) hx(path string, values url.Values) (*http.Response, string) {
	return c.do(http.MethodPost, path, strings.NewReader(values.Encode()), "application/x-www-form-urlencoded", true)
}

func (c *client) login(login, password string) {
	c.t.Helper()
	res, body := c.form("/login", url.Values{"login": {login}, "password": {password}})
	if res.StatusCode != http.StatusSeeOther || c.cookie("gochat_session") == "" {
		c.t.Fatalf("%s: login failed: %d %s", c.name, res.StatusCode, body)
	}
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type socket struct {
	t    *testing.T
	conn *websocket.Conn
	in   chan string
}

func (c *client) socket(origin string) (*socket, *http.Response, error) {
	h := http.Header{}
	h.Set("Origin", origin)
	u, _ := url.Parse(baseURL)
	h.Set("Cookie", "gochat_session="+c.cookie("gochat_session"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, res, err := websocket.Dial(ctx, "ws://"+u.Host+"/ws", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, res, err
	}
	// A sidebar of many channels outgrows the library's default limit.
	conn.SetReadLimit(4 << 20)
	s := &socket{t: c.t, conn: conn, in: make(chan string, 256)}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				close(s.in)
				return
			}
			s.in <- string(data)
		}
	}()
	return s, res, nil
}

func (s *socket) send(v any) {
	s.t.Helper()
	b, _ := json.Marshal(v)
	if err := s.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		s.t.Fatal(err)
	}
}

// expect waits for a frame containing every one of parts.
func (s *socket) expect(within time.Duration, parts ...string) string {
	s.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-s.in:
			if !ok {
				s.t.Fatalf("socket closed waiting for %q", parts)
			}
			all := true
			for _, p := range parts {
				if !strings.Contains(f, p) {
					all = false
				}
			}
			if all {
				return f
			}
		case <-deadline:
			s.t.Fatalf("no frame with %q", parts)
		}
	}
}

// collect returns the frames that arrive within a while.
func (s *socket) collect(within time.Duration) []string {
	var out []string
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-s.in:
			if !ok {
				return out
			}
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
}

// closed waits for the server to close the socket.
func (s *socket) closed(within time.Duration) {
	s.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case _, ok := <-s.in:
			if !ok {
				return
			}
		case <-deadline:
			s.t.Fatal("the socket is still open")
		}
	}
}

func (s *socket) refuse(within time.Duration, part string) {
	s.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-s.in:
			if !ok {
				return
			}
			if strings.Contains(f, part) {
				s.t.Fatalf("got a frame with %q: %s", part, f)
			}
		case <-deadline:
			return
		}
	}
}

var (
	inviteRe    = regexp.MustCompile(`/join/([A-Za-z0-9_-]{43})`)
	channelRe   = regexp.MustCompile(`/t/[a-z0-9-]+/ch/([0-9a-f-]{36})`)
	attRe       = regexp.MustCompile(`name="attachment" value="([0-9a-f-]{36})"`)
	secretRe    = regexp.MustCompile(`<code>([A-Z2-7]{16,})</code>`)
	codeRe      = regexp.MustCompile(`<code>([a-z2-7]{5}-[a-z2-7]{5})</code>`)
	teamIDRe    = regexp.MustCompile(`data-team="([0-9a-f-]{36})"`)
	msgIDRe     = regexp.MustCompile(`id="msg-([0-9a-f-]{36})"`)
	newestRe    = regexp.MustCompile(`data-newest="([0-9a-f-]{36})"`)
	offTopicRe  = regexp.MustCompile(`data-channel="([0-9a-f-]{36})"(?s:[^<]*>.{0,600}?)<span class="label">Off-Topic</span>`)
	photoRe     = regexp.MustCompile(`/photos/([ut])/([0-9a-f-]{36})/([0-9a-f]{16})`)
	staticCSSRe = regexp.MustCompile(`/static/[0-9a-f]+/app\.css`)
	pickRe      = regexp.MustCompile(`data-pick="([0-9a-f-]{36})"`)
	adminUserRe = regexp.MustCompile(`/admin/users/([0-9a-f-]{36})/`)
	sessionRe   = regexp.MustCompile(`/settings/sessions/([0-9a-f]{24})/revoke`)
)

// bootstrap sets a fresh server up with the admin and the team the tests use.
func bootstrap(t *testing.T, admin *client) {
	t.Helper()
	if setupKey == "" {
		t.Fatal("GOCHAT_E2E_SETUP_TOKEN is not set")
	}
	if _, page := admin.get("/signup"); !strings.Contains(page, `name="setup_token"`) {
		t.Fatalf("the first sign-up asks for no setup token: %s", page)
	}
	form := url.Values{
		"username": {adminName}, "display_name": {"Ada Admin"}, "email": {adminName + "@example.test"},
		"password": {adminPass}, "confirm": {adminPass},
	}
	if res, body := admin.form("/signup", form); res.StatusCode == http.StatusSeeOther || !strings.Contains(body, "the setup token is not correct") {
		t.Fatalf("the first account was created without the setup token: %d %s", res.StatusCode, body)
	}
	form.Set("setup_token", setupKey)
	res, body := admin.form("/signup", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("bootstrap sign-up: %d %s", res.StatusCode, body)
	}
	res, body = admin.form("/teams", url.Values{"name": {teamName}, "display_name": {"Acme"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("bootstrap team: %d %s", res.StatusCode, body)
	}
}

// join signs a new person up with an invite from admin.
func join(t *testing.T, admin *client, prefix string) (c *client, name, pass string) {
	t.Helper()
	_, body := admin.hx("/t/"+teamName+"/invites", url.Values{})
	m := inviteRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no invite link in %s", body)
	}
	c = newClient(t, prefix)
	name, pass = prefix+"-"+random(4), "pw-"+random(8)
	c.get("/signup?invite=" + m[1])
	if res, body := c.form("/signup", url.Values{
		"username": {name}, "display_name": {prefix}, "email": {name + "@example.test"},
		"password": {pass}, "confirm": {pass}, "invite": {m[1]},
	}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%s's signup: %d %s", prefix, res.StatusCode, body)
	}
	return c, name, pass
}

// adminFind returns the id of the user the admin page finds for login.
func adminFind(t *testing.T, admin *client, login string) string {
	t.Helper()
	_, page := admin.get("/admin/users?q=" + url.QueryEscape(login))
	m := adminUserRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the admin page did not find %s: %s", login, page)
	}
	return m[1]
}

// signedOut reports whether c's session no longer works.
func signedOut(c *client) bool {
	res, _ := c.get("/settings")
	return res.StatusCode == http.StatusSeeOther && strings.HasPrefix(res.Header.Get("Location"), "/login")
}

func TestEndToEnd(t *testing.T) {
	if baseURL == "" {
		t.Skip("GOCHAT_E2E_URL is not set")
	}
	admin := newClient(t, "admin")
	if os.Getenv("GOCHAT_E2E_BOOTSTRAP") != "" {
		bootstrap(t, admin)
	}
	admin.login(adminName, adminPass)

	// One-time invite.
	_, body := admin.hx("/t/"+teamName+"/invites", url.Values{})
	m := inviteRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no invite link in %s", body)
	}
	invite := m[1]

	bob := newClient(t, "bob")
	bobName, bobPass := "bob-"+random(4), "pw-"+random(8)
	if _, body := bob.get("/join/" + invite); !strings.Contains(body, "Create an account") {
		t.Fatalf("join page: %s", body)
	}
	bob.get("/signup?invite=" + invite)
	res, body := bob.form("/signup", url.Values{
		"username": {bobName}, "display_name": {"Bob <b>Tester</b>"}, "email": {bobName + "@example.test"},
		"password": {bobPass}, "confirm": {bobPass}, "invite": {invite},
	})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/t/"+teamName {
		t.Fatalf("signup with invite: %d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}

	t.Run("invite works once", func(t *testing.T) {
		eve := newClient(t, "eve")
		eve.get("/signup?invite=" + invite)
		res, body := eve.form("/signup", url.Values{
			"username": {"eve-" + random(4)}, "email": {"eve-" + random(4) + "@example.test"},
			"password": {"pw-" + random(8)}, "confirm": {"x"}, "invite": {invite},
		})
		if res.StatusCode == http.StatusSeeOther {
			t.Fatal("a used invite admitted a second account")
		}
		if !strings.Contains(body, "Invite required") {
			t.Fatalf("expected the invite to be spent: %s", body)
		}
	})

	res, _ = bob.get("/t/" + teamName)
	m = channelRe.FindStringSubmatch(res.Header.Get("Location"))
	if m == nil {
		t.Fatalf("team home did not redirect to a channel: %s", res.Header.Get("Location"))
	}
	town := m[1]
	_, page := admin.get("/t/" + teamName + "/ch/" + town)
	teamID := teamIDRe.FindStringSubmatch(page)[1]

	adminWS, _, err := admin.socket(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	bobWS, _, err := bob.socket(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	view := func(s *socket, ch string) {
		s.send(map[string]string{"op": "view", "team_id": teamID, "team_name": teamName, "channel_id": ch})
	}
	view(adminWS, town)
	view(bobWS, town)
	time.Sleep(300 * time.Millisecond)

	t.Run("messages arrive in real time and escaped", func(t *testing.T) {
		bobWS.send(map[string]string{"op": "post", "channel_id": town, "content": "hello <script>alert(1)</script> from bob"})
		f := adminWS.expect(5*time.Second, "from bob", `hx-swap-oob="afterbegin"`)
		if strings.Contains(f, "<script>") || strings.Contains(f, "<b>Tester") {
			t.Fatalf("unescaped content reached the page: %s", f)
		}
		bobWS.expect(5*time.Second, "from bob")
	})

	t.Run("threads keep replies apart", func(t *testing.T) {
		bobWS.send(map[string]string{"op": "post", "channel_id": town, "content": "root for a thread"})
		f := adminWS.expect(5*time.Second, "root for a thread", `hx-swap-oob="afterbegin"`)
		root := msgIDRe.FindStringSubmatch(f)[1]
		bobWS.send(map[string]string{"op": "thread", "root_id": root})
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "root_id": root, "content": "first reply"})
		f = bobWS.expect(5*time.Second, "first reply", `id="replies-`+root+`"`)
		reply := msgIDRe.FindStringSubmatch(f)[1]
		adminWS.expect(5*time.Second, `id="msg-`+root+`"`, "1 reply")
		if _, page := admin.get("/t/" + teamName + "/ch/" + town); strings.Contains(page, "first reply") || !strings.Contains(page, "1 reply") {
			t.Fatal("the reply is in the channel's stream, or the root does not count it")
		}
		if _, panel := admin.do(http.MethodGet, "/m/"+town+"/"+root+"/thread", nil, "", true); !strings.Contains(panel, "root for a thread") ||
			!strings.Contains(panel, "first reply") || !strings.Contains(panel, `id="reply-composer"`) {
			t.Fatalf("thread panel: %s", panel)
		}
		if res, _ := bob.hx("/m/"+town+"/"+reply+"/delete?root="+root, url.Values{}); res.StatusCode != http.StatusForbidden {
			t.Fatalf("bob deleted admin's reply: %d", res.StatusCode)
		}
		if res, _ := admin.hx("/m/"+town+"/"+reply+"/delete?root="+root, url.Values{}); res.StatusCode != http.StatusOK {
			t.Fatalf("deleting a reply: %d", res.StatusCode)
		}
		bobWS.expect(5*time.Second, `id="msg-`+reply+`"`, `hx-swap-oob="delete"`)
		if f := adminWS.expect(5*time.Second, `id="msg-`+root+`"`); strings.Contains(f, "thread-link") {
			t.Fatalf("the root still counts a deleted reply: %s", f)
		}
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "root_id": root, "content": "second reply"})
		bobWS.expect(5*time.Second, "second reply")
		if res, _ := bob.hx("/m/"+town+"/"+root+"/delete", url.Values{}); res.StatusCode != http.StatusOK {
			t.Fatalf("deleting a root: %d", res.StatusCode)
		}
		bobWS.expect(5*time.Second, `id="thread"`, `hx-swap-oob="outerHTML"`)
		if res, _ := admin.do(http.MethodGet, "/m/"+town+"/"+root+"/thread", nil, "", true); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a deleted root's thread: %d", res.StatusCode)
		}
		bobWS.send(map[string]string{"op": "post", "channel_id": town, "root_id": root, "content": "late reply"})
		bobWS.expect(5*time.Second, "message not found")
	})

	t.Run("mentions highlight, count and complete", func(t *testing.T) {
		_, page := bob.get("/t/" + teamName + "/ch/" + town)
		m := offTopicRe.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no Off-Topic in bob's sidebar: %s", page)
		}
		offTopic := m[1]
		adminWS.send(map[string]string{"op": "post", "channel_id": offTopic, "content": "please look, @" + bobName})
		bobWS.expect(5*time.Second, `id="unread-`+offTopic+`" class="mentions"`, ">1<")
		if _, page := bob.get("/t/" + teamName + "/ch/" + town); !strings.Contains(page, `id="unread-`+offTopic+`" class="mentions"`) {
			t.Fatal("the mention is not counted on reload")
		}
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "content": "hey @" + bobName + " and `@nobody`"})
		f := bobWS.expect(5*time.Second, `class="msg mentioned"`)
		if !strings.Contains(f, `<span class="mention">@`+bobName+`</span>`) || strings.Contains(f, `">@nobody`) {
			t.Fatalf("mention markup: %s", f)
		}
		if _, list := bob.do(http.MethodGet, "/m/"+town+"/people?q=adm", nil, "", true); !strings.Contains(list, `data-insert="@admin "`) {
			t.Fatalf("suggestions for adm: %s", list)
		}
		if _, list := bob.do(http.MethodGet, "/m/"+town+"/people?q=cha", nil, "", true); !strings.Contains(list, `data-insert="@channel "`) {
			t.Fatalf("suggestions for cha: %s", list)
		}
		if res, _ := bob.do(http.MethodGet, "/m/"+town+"/people?q=%3Cx", nil, "", true); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a bad prefix was accepted: %d", res.StatusCode)
		}
		// Reading the channel clears its count: the page tells the socket
		// the newest message it shows.
		defer view(bobWS, town)
		_, page = bob.get("/t/" + teamName + "/ch/" + offTopic)
		newest := newestRe.FindStringSubmatch(page)
		if newest == nil {
			t.Fatalf("no newest message on the page: %s", page)
		}
		bobWS.send(map[string]string{"op": "view", "team_id": teamID, "team_name": teamName, "channel_id": offTopic, "last_id": newest[1]})
		deadline := time.Now().Add(6 * time.Second)
		for {
			_, page := bob.get("/t/" + teamName + "/ch/" + town)
			if !strings.Contains(page, `id="unread-`+offTopic+`" class="mentions"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("reading the channel did not clear its mentions")
			}
			time.Sleep(300 * time.Millisecond)
		}
	})

	t.Run("reactions toggle and arrive live", func(t *testing.T) {
		bobWS.send(map[string]string{"op": "post", "channel_id": town, "content": "react to me"})
		f := adminWS.expect(5*time.Second, "react to me", `hx-swap-oob="afterbegin"`)
		msg := msgIDRe.FindStringSubmatch(f)[1]
		bar := `id="reactions-` + msg + `"`
		react := func(c *client, emoji string, remove bool) (*http.Response, string) {
			v := url.Values{"emoji": {emoji}}
			if remove {
				v.Set("remove", "1")
			}
			return c.hx("/m/"+town+"/"+msg+"/react", v)
		}
		if res, body := react(admin, "tada", false); res.StatusCode != http.StatusOK || !strings.Contains(body, bar) || !strings.Contains(body, `aria-pressed="true"`) {
			t.Fatalf("react: %d %s", res.StatusCode, body)
		}
		bobWS.expect(5*time.Second, bar, "🎉", `<span class="n">1</span>`, `aria-pressed="false"`)
		react(bob, "tada", false)
		adminWS.expect(5*time.Second, bar, `<span class="n">2</span>`, `aria-pressed="true"`)
		react(admin, "tada", true)
		bobWS.expect(5*time.Second, bar, `<span class="n">1</span>`, `aria-pressed="true"`)
		if _, page := admin.get("/t/" + teamName + "/ch/" + town); !strings.Contains(page, bar) || !strings.Contains(page, "🎉") {
			t.Fatal("the reaction is gone after a reload")
		}
		if res, _ := react(bob, "<script>", false); res.StatusCode == http.StatusOK {
			t.Fatal("an unknown emoji was accepted")
		}
		if _, picker := bob.do(http.MethodGet, "/m/"+town+"/"+msg+"/react", nil, "", true); !strings.Contains(picker, `class="emoji-grid"`) {
			t.Fatalf("picker: %s", picker)
		}
	})

	t.Run("notifications follow mentions, replies and mutes", func(t *testing.T) {
		_, page := bob.get("/t/" + teamName + "/ch/" + town)
		offTopic := offTopicRe.FindStringSubmatch(page)[1]
		adminWS.send(map[string]string{"op": "post", "channel_id": offTopic, "content": "a word, @" + bobName})
		f := bobWS.expect(5*time.Second, `id="notify"`, `data-url="/c/`+offTopic+`"`)
		if !strings.Contains(f, "in Off-Topic") || !strings.Contains(f, "a word, @"+bobName) {
			t.Fatalf("notice: %s", f)
		}

		bobWS.send(map[string]string{"op": "post", "channel_id": town, "content": "thread for notices"})
		root := msgIDRe.FindStringSubmatch(adminWS.expect(5*time.Second, "thread for notices", `hx-swap-oob="afterbegin"`))[1]
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "root_id": root, "content": "a reply for bob"})
		if f := bobWS.expect(5*time.Second, `id="notify"`, "replied in a thread"); !strings.Contains(f, "?thread="+root) {
			t.Fatalf("reply notice: %s", f)
		}

		if res, body := bob.hx("/t/"+teamName+"/ch/"+offTopic+"/mute", url.Values{"muted": {"1"}}); res.StatusCode != http.StatusOK || !strings.Contains(body, `aria-pressed="true"`) {
			t.Fatalf("mute: %d %s", res.StatusCode, body)
		}
		bobWS.expect(5*time.Second, `id="channel-lists"`, "sb-link muted")
		adminWS.send(map[string]string{"op": "post", "channel_id": offTopic, "content": "again, @" + bobName})
		frames := strings.Join(bobWS.collect(2*time.Second), "\n")
		if strings.Contains(frames, `id="notify"`) || !strings.Contains(frames, `id="unread-`+offTopic+`" class="mentions"`) {
			t.Fatalf("a muted channel notified, or did not count the mention: %s", frames)
		}
		bob.hx("/t/"+teamName+"/ch/"+offTopic+"/mute", url.Values{"muted": {"0"}})

		res, _ := bob.get("/c/" + offTopic + "?thread=" + root)
		if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || loc != "/t/"+teamName+"/ch/"+offTopic+"?thread="+root {
			t.Fatalf("channel link: %d %q", res.StatusCode, loc)
		}
	})

	t.Run("typing reaches others only", func(t *testing.T) {
		adminWS.send(map[string]string{"op": "typing", "channel_id": town})
		bobWS.expect(5*time.Second, "is typing")
		adminWS.refuse(time.Second, "is typing")
	})

	t.Run("attachments upload, post and download", func(t *testing.T) {
		png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 64))
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "../../pic<1>.png")
		_, _ = fw.Write(png)
		_ = mw.Close()
		res, body := bob.do(http.MethodPost, "/upload?channel="+town, &buf, mw.FormDataContentType(), true)
		m := attRe.FindStringSubmatch(body)
		if res.StatusCode != http.StatusOK || m == nil {
			t.Fatalf("upload: %d %s", res.StatusCode, body)
		}
		att := m[1]
		bobWS.send(map[string]any{"op": "post", "channel_id": town, "content": "", "attachment": att})
		adminWS.expect(5*time.Second, "/files/"+att, "pic")
		res, got := admin.get("/files/" + att)
		if res.StatusCode != http.StatusOK || got != string(png) {
			t.Fatalf("download: %d", res.StatusCode)
		}
		if res.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("download headers: %v", res.Header)
		}
		if res, _ := admin.get("/files/" + att + "/inline"); res.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("inline image type: %s", res.Header.Get("Content-Type"))
		}
		// An attachment is posted once.
		bobWS.send(map[string]any{"op": "post", "channel_id": town, "content": "again", "attachment": att})
		bobWS.expect(5*time.Second, "attachment is not available")
	})

	t.Run("voice messages follow the channel's setting", func(t *testing.T) {
		webm := []byte("\x1a\x45\xdf\xa3" + strings.Repeat("\x00", 64))
		upload := func(c *client, data []byte, duration string) (*http.Response, string) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, _ := mw.CreateFormFile("file", "voice-message.webm")
			_, _ = fw.Write(data)
			_ = mw.Close()
			return c.do(http.MethodPost, "/upload?channel="+town+"&voice=1&duration="+duration, &buf, mw.FormDataContentType(), true)
		}
		// Whatever an earlier run left, the channel starts with voice on.
		settings := "/t/" + teamName + "/ch/" + town + "/voice"
		if res, _ := admin.hx(settings, url.Values{"enabled": {"1"}}); res.StatusCode != http.StatusOK {
			t.Fatalf("turn voice on: %d", res.StatusCode)
		}
		// Drop the update that may have caused, so it cannot pass for a later one.
		bobWS.collect(500 * time.Millisecond)
		res, body := upload(bob, webm, "2500")
		m := attRe.FindStringSubmatch(body)
		if res.StatusCode != http.StatusOK || m == nil || !strings.Contains(body, "Voice message") {
			t.Fatalf("voice upload: %d %s", res.StatusCode, body)
		}
		att := m[1]
		bobWS.send(map[string]any{"op": "post", "channel_id": town, "content": "", "attachment": att})
		adminWS.expect(5*time.Second, "voice-player", `data-duration="2500"`, "/files/"+att+"/inline")
		res, got := admin.get("/files/" + att + "/inline")
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "audio/webm" || got != string(webm) {
			t.Fatalf("voice playback: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
		}
		// Browsers play media in ranges; Safari plays nothing without them.
		size := strconv.Itoa(len(webm))
		if res, got := admin.getRange("/files/"+att+"/inline", "bytes=0-1"); res.StatusCode != http.StatusPartialContent ||
			res.Header.Get("Content-Range") != "bytes 0-1/"+size || got != string(webm[:2]) {
			t.Fatalf("a range of the voice message: %d %q %q", res.StatusCode, res.Header.Get("Content-Range"), got)
		}
		if res, got := admin.getRange("/files/"+att+"/inline", "bytes=4-"); res.StatusCode != http.StatusPartialContent || got != string(webm[4:]) {
			t.Fatalf("the rest of the voice message: %d %d bytes", res.StatusCode, len(got))
		}
		if res, _ := admin.getRange("/files/"+att+"/inline", "bytes="+size+"-"); res.StatusCode != http.StatusRequestedRangeNotSatisfiable ||
			res.Header.Get("Content-Range") != "bytes */"+size {
			t.Fatalf("a range past the end: %d %q", res.StatusCode, res.Header.Get("Content-Range"))
		}
		if res, _ := upload(bob, []byte("not a recording"), "1000"); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("a voice message that is no recording: %d", res.StatusCode)
		}

		if res, _ := bob.hx(settings, url.Values{"enabled": {"0"}}); res.StatusCode != http.StatusForbidden {
			t.Fatalf("a member turned voice off: %d", res.StatusCode)
		}
		if res, body := admin.hx(settings, url.Values{"enabled": {"0"}}); res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved.") {
			t.Fatalf("turn voice off: %d %s", res.StatusCode, body)
		}
		if got := bobWS.expect(5*time.Second, `data-voice-for="`+town+`"`); strings.Contains(got, "voice-record") {
			t.Fatalf("the composer still offers voice: %s", got)
		}
		if res, _ := upload(bob, webm, "1000"); res.StatusCode != http.StatusConflict {
			t.Fatalf("voice upload where voice is off: %d", res.StatusCode)
		}
		if res, _ := admin.hx(settings, url.Values{"enabled": {"1"}}); res.StatusCode != http.StatusOK {
			t.Fatalf("turn voice on: %d", res.StatusCode)
		}
		bobWS.expect(5*time.Second, `data-voice-for="`+town+`"`, "voice-record")
	})

	t.Run("voice channels: talk, mute, moderators and refusals", func(t *testing.T) {
		testVoiceChannels(t, admin, bob, adminWS, bobWS)
	})

	t.Run("private channels stay private", func(t *testing.T) {
		name := "secret-" + random(3)
		res, body := admin.hx("/t/"+teamName+"/channels", url.Values{"name": {name}, "display_name": {"Secret"}, "type": {"private"}})
		m := channelRe.FindStringSubmatch(res.Header.Get("HX-Location"))
		if m == nil {
			t.Fatalf("create private channel: %d %s", res.StatusCode, body)
		}
		secret := m[1]
		if res, _ := bob.get("/t/" + teamName + "/ch/" + secret); res.StatusCode != http.StatusNotFound {
			t.Fatalf("bob read a private channel: %d", res.StatusCode)
		}
		if res, _ := bob.get("/t/" + teamName + "/ch/" + secret + "/messages?before=" + town); res.StatusCode != http.StatusNotFound {
			t.Fatalf("bob listed a private channel: %d", res.StatusCode)
		}
		bobWS.send(map[string]string{"op": "post", "channel_id": secret, "content": "sneaky"})
		bobWS.expect(5*time.Second, "channel not found")
		view(adminWS, secret)
		adminWS.send(map[string]string{"op": "post", "channel_id": secret, "content": "for admins only"})
		adminWS.expect(5*time.Second, "for admins only")
		bobWS.refuse(time.Second, "for admins only")
		view(adminWS, town)
	})

	t.Run("search finds what each person may read", func(t *testing.T) {
		word := "zq" + random(4)
		name := "hidden-" + random(3)
		res, _ := admin.hx("/t/"+teamName+"/channels", url.Values{"name": {name}, "display_name": {"Hidden"}, "type": {"private"}})
		hidden := channelRe.FindStringSubmatch(res.Header.Get("HX-Location"))[1]
		view(adminWS, hidden)
		adminWS.send(map[string]string{"op": "post", "channel_id": hidden, "content": "private " + word + " plans"})
		adminWS.expect(5*time.Second, "private "+word)
		view(adminWS, town)
		bobWS.send(map[string]string{"op": "post", "channel_id": town, "content": "the public " + word + " release notes <b>bold</b>"})
		pub := msgIDRe.FindStringSubmatch(adminWS.expect(5*time.Second, "public "+word))[1]

		search := func(c *client, q string) string {
			t.Helper()
			for range 10 {
				_, body := c.do(http.MethodGet, "/t/"+teamName+"/search?q="+url.QueryEscape(q), nil, "", true)
				// The results, without the query echoed in the search box.
				_, body, _ = strings.Cut(body, `id="search-results"`)
				if !strings.Contains(body, "searching too fast") {
					return body
				}
				time.Sleep(time.Second)
			}
			t.Fatalf("searching %q stayed too fast", q)
			return ""
		}
		// Messages are filed for search a moment after they are posted.
		var body string
		for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(400 * time.Millisecond) {
			if body = search(admin, word); strings.Contains(body, "plans") && strings.Contains(body, "release") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("search did not find both messages: %s", body)
			}
		}
		if !strings.Contains(body, "<mark>"+word+"</mark>") || strings.Contains(body, "<b>bold") || !strings.Contains(body, "?msg="+pub) {
			t.Fatalf("results: %s", body)
		}
		if body := search(bob, word); !strings.Contains(body, "release") || strings.Contains(body, "plans") {
			t.Fatalf("bob's results: %s", body)
		}
		for q, want := range map[string]string{
			`"release notes" ` + word:   "release",
			word + " from:" + adminName: "plans",
			word + " in:" + name:        "plans",
			word + " -release":          "plans",
		} {
			body := search(admin, q)
			other := map[string]string{"release": "plans", "plans": "release"}[want]
			if !strings.Contains(body, want) || strings.Contains(body, other) {
				t.Fatalf("%q: %s", q, body)
			}
		}
		if body := search(admin, "the"); !strings.Contains(body, "Type a word") {
			t.Fatalf("a stop word alone: %s", body)
		}
		if _, page := admin.get("/t/" + teamName + "/ch/" + town + "?msg=" + pub); !strings.Contains(page, ` focus" id="msg-`+pub+`" data-focus="1"`) {
			t.Fatalf("the linked message is not focused: %s", page)
		}
		if res, _ := bob.hx("/m/"+town+"/"+pub, url.Values{"content": {"nothing to see"}}); res.StatusCode != http.StatusOK {
			t.Fatalf("edit: %d", res.StatusCode)
		}
		for deadline := time.Now().Add(8 * time.Second); strings.Contains(search(admin, word), "release"); time.Sleep(400 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("an edited message is still found by its old words")
			}
		}
	})

	t.Run("group conversations and the switcher", func(t *testing.T) {
		_, body := admin.hx("/t/"+teamName+"/invites", url.Values{})
		carol := newClient(t, "carol")
		carolName, carolPass := "carol-"+random(4), "pw-"+random(8)
		inv := inviteRe.FindStringSubmatch(body)[1]
		carol.get("/signup?invite=" + inv)
		if res, body := carol.form("/signup", url.Values{
			"username": {carolName}, "display_name": {"Carol"}, "email": {carolName + "@example.test"},
			"password": {carolPass}, "confirm": {carolPass}, "invite": {inv},
		}); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("carol's signup: %d %s", res.StatusCode, body)
		}
		userID := func(q string) string {
			_, list := admin.do(http.MethodGet, "/t/"+teamName+"/people/search?q="+q, nil, "", true)
			m := pickRe.FindStringSubmatch(list)
			if m == nil {
				t.Fatalf("no one found for %q: %s", q, list)
			}
			return m[1]
		}
		bobUser, carolUser := userID(bobName), userID(carolName)
		open := func(people ...string) string {
			t.Helper()
			res, body := admin.hx("/t/"+teamName+"/dm", url.Values{"user_id": people})
			m := channelRe.FindStringSubmatch(res.Header.Get("HX-Location"))
			if m == nil {
				t.Fatalf("open %v: %d %s", people, res.StatusCode, body)
			}
			return m[1]
		}
		group := open(bobUser, carolUser)
		if again := open(carolUser, bobUser); again != group {
			t.Fatalf("the same people opened another group: %s, %s", group, again)
		}
		if direct := open(bobUser); direct == group {
			t.Fatal("one person opened the group")
		}
		bobWS.expect(5*time.Second, `id="channel-lists"`, `class="group-count"`)
		view(adminWS, group)
		adminWS.send(map[string]string{"op": "post", "channel_id": group, "content": "hello group"})
		bobWS.expect(5*time.Second, `id="notify"`, "hello group")
		bobWS.expect(5*time.Second, `id="unread-`+group+`" class="mentions"`)
		view(adminWS, town)
		if _, page := carol.get("/t/" + teamName + "/ch/" + group); !strings.Contains(page, "hello group") || !strings.Contains(page, "Your conversation with") {
			t.Fatalf("carol's view of the group: %s", page)
		}
		if res, _ := bob.hx("/t/"+teamName+"/ch/"+group+"/leave", url.Values{}); res.StatusCode < 400 {
			t.Fatalf("bob left a group conversation: %d", res.StatusCode)
		}
		if res, _ := admin.hx("/t/"+teamName+"/dm", url.Values{"user_id": {bobUser, carolUser, "00000000-0000-4000-8000-000000000000"}}); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a group with a stranger: %d", res.StatusCode)
		}

		if _, list := bob.do(http.MethodGet, "/t/"+teamName+"/switch/results?q=off", nil, "", true); !strings.Contains(list, "Off-Topic") || !strings.Contains(list, `role="option"`) {
			t.Fatalf("switcher for off: %s", list)
		}
		if _, list := bob.do(http.MethodGet, "/t/"+teamName+"/switch/results?q=carol", nil, "", true); !strings.Contains(list, "Carol") {
			t.Fatalf("switcher for carol: %s", list)
		}
		if _, modal := bob.do(http.MethodGet, "/t/"+teamName+"/switch", nil, "", true); !strings.Contains(modal, "data-switch-input") || !strings.Contains(modal, "Town Square") {
			t.Fatalf("switcher: %s", modal)
		}
	})

	t.Run("channel settings, privacy and archiving", func(t *testing.T) {
		res, _ := admin.hx("/t/"+teamName+"/channels", url.Values{"name": {"proj-" + random(3)}, "display_name": {"Project"}, "type": {"public"}})
		proj := channelRe.FindStringSubmatch(res.Header.Get("HX-Location"))[1]
		base := "/t/" + teamName + "/ch/" + proj
		bob.hx(base+"/join", url.Values{})
		view(bobWS, proj)
		defer view(bobWS, town)
		if res, _ := bob.hx(base+"/settings", url.Values{"display_name": {"Mine"}, "name": {"mine-" + random(3)}}); res.StatusCode != http.StatusForbidden {
			t.Fatalf("a member changed the settings: %d", res.StatusCode)
		}
		newName := "launch-" + random(3)
		res, body := admin.hx(base+"/settings", url.Values{"display_name": {"Launch"}, "name": {newName}, "purpose": {"Ship it"}})
		if res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved.") {
			t.Fatalf("settings: %d %s", res.StatusCode, body)
		}
		bobWS.expect(5*time.Second, `id="channel-header-`+proj+`"`, "Launch", "Ship it")
		if _, body := admin.hx("/t/"+teamName+"/channels", url.Values{"name": {newName}, "display_name": {"Again"}, "type": {"public"}}); !strings.Contains(body, "exists") {
			t.Fatalf("a renamed channel's name was free: %s", body)
		}
		if _, body := admin.hx(base+"/privacy", url.Values{"private": {"1"}}); !strings.Contains(body, "private now") {
			t.Fatalf("make private: %s", body)
		}
		if res, _ := bob.hx(base+"/privacy", url.Values{"private": {"0"}}); res.StatusCode != http.StatusForbidden {
			t.Fatalf("a member made the channel public: %d", res.StatusCode)
		}
		if res, _ := admin.hx(base+"/archive", url.Values{"archived": {"1"}}); res.Header.Get("HX-Location") == "" {
			t.Fatalf("archive: %d", res.StatusCode)
		}
		bobWS.expect(5*time.Second, `id="composer-area-`+proj+`"`, "is archived")
		bobWS.send(map[string]string{"op": "post", "channel_id": proj, "content": "too late"})
		bobWS.expect(5*time.Second, "this channel is archived")
		if _, page := bob.get("/t/" + teamName + "/ch/" + town); strings.Contains(page, `data-channel="`+proj+`"`) {
			t.Fatal("an archived channel stayed in the sidebar")
		}
		if _, modal := bob.do(http.MethodGet, "/t/"+teamName+"/channels/browse", nil, "", true); !strings.Contains(modal, "Archived channels") || !strings.Contains(modal, "Launch") {
			t.Fatalf("browse: %s", modal)
		}
		admin.hx(base+"/archive", url.Values{"archived": {"0"}})
		bobWS.expect(5*time.Second, `id="composer-area-`+proj+`"`, `id="composer"`)
		if res, _ := admin.hx("/t/"+teamName+"/ch/"+town+"/archive", url.Values{"archived": {"1"}}); res.StatusCode < 400 {
			t.Fatalf("the town square was archived: %d", res.StatusCode)
		}
	})

	t.Run("threads gather what people follow", func(t *testing.T) {
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "content": "inbox root " + random(3)})
		root := msgIDRe.FindStringSubmatch(bobWS.expect(5*time.Second, "inbox root", `hx-swap-oob="afterbegin"`))[1]
		card := `hx-get="/m/` + town + `/` + root + `/thread"`
		bobWS.send(map[string]string{"op": "post", "channel_id": town, "root_id": root, "content": "inbox reply"})
		adminWS.expect(5*time.Second, `id="threads-unread" class="dot on"`)
		threads := func(c *client) string {
			_, body := c.do(http.MethodGet, "/t/"+teamName+"/threads", nil, "", true)
			return body
		}
		if list := threads(admin); !strings.Contains(list, `thread-card unread" `+card) {
			t.Fatalf("admin's threads: %s", list)
		}
		if list := threads(bob); !strings.Contains(list, card) || strings.Contains(list, `thread-card unread" `+card) {
			t.Fatalf("the replier's threads: %s", list)
		}
		if _, panel := admin.do(http.MethodGet, "/m/"+town+"/"+root+"/thread", nil, "", true); !strings.Contains(panel, `aria-pressed="true"`) || !strings.Contains(panel, "Following") {
			t.Fatalf("thread panel: %s", panel)
		}
		if list := threads(admin); strings.Contains(list, `thread-card unread" `+card) {
			t.Fatal("opening the thread did not read it")
		}
		adminWS.send(map[string]string{"op": "view", "team_id": teamID, "team_name": teamName, "channel_id": ""})
		time.Sleep(200 * time.Millisecond)
		bobWS.send(map[string]string{"op": "post", "channel_id": town, "root_id": root, "content": "another reply"})
		adminWS.expect(5*time.Second, `id="threads-refresh"`, "threads?list=1")
		view(adminWS, town)
		if res, body := bob.hx("/m/"+town+"/"+root+"/follow", url.Values{"follow": {"0"}}); res.StatusCode != http.StatusOK || !strings.Contains(body, ">Follow<") {
			t.Fatalf("unfollow: %d %s", res.StatusCode, body)
		}
		if list := threads(bob); strings.Contains(list, card) {
			t.Fatal("an unfollowed thread is still listed")
		}
		bob.hx("/m/"+town+"/"+root+"/follow", url.Values{"follow": {"1"}})
		if list := threads(bob); !strings.Contains(list, card) {
			t.Fatal("a followed thread is not listed")
		}
		if res, page := admin.get("/t/" + teamName + "/threads"); res.StatusCode != http.StatusOK || !strings.Contains(page, `id="threads-view"`) || !strings.Contains(page, `data-place="threads"`) {
			t.Fatalf("threads page: %d", res.StatusCode)
		}
	})

	t.Run("pins are shared and saves are private", func(t *testing.T) {
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "content": "pin me " + random(3)})
		id := msgIDRe.FindStringSubmatch(bobWS.expect(5*time.Second, "pin me", `hx-swap-oob="afterbegin"`))[1]
		if res, body := bob.hx("/m/"+town+"/"+id+"/pin", url.Values{"pinned": {"1"}}); res.StatusCode != http.StatusOK || !strings.Contains(body, "Pinned</span>") {
			t.Fatalf("pin: %d %s", res.StatusCode, body)
		}
		adminWS.expect(5*time.Second, `id="pins-btn-`+town+`"`, `class="count"`)
		adminWS.expect(5*time.Second, `id="msg-`+id+`"`, "Pinned</span>")
		pins := func() string {
			_, body := admin.do(http.MethodGet, "/t/"+teamName+"/ch/"+town+"/pins", nil, "", true)
			return body
		}
		if list := pins(); !strings.Contains(list, "?msg="+id) {
			t.Fatalf("pinned list: %s", list)
		}
		if res, body := admin.hx("/m/"+town+"/"+id+"/save", url.Values{"saved": {"1"}}); res.StatusCode != http.StatusOK || !strings.Contains(body, `flag saved`) {
			t.Fatalf("save: %d %s", res.StatusCode, body)
		}
		adminWS.expect(5*time.Second, `id="saved-changed"`)
		saved := func(c *client) string {
			_, body := c.do(http.MethodGet, "/t/"+teamName+"/saved", nil, "", true)
			return body
		}
		if list := saved(admin); !strings.Contains(list, "?msg="+id) {
			t.Fatalf("admin's saved: %s", list)
		}
		if list := saved(bob); strings.Contains(list, "?msg="+id) {
			t.Fatal("a save showed to someone else")
		}
		_, page := admin.do(http.MethodGet, "/t/"+teamName+"/ch/"+town, nil, "", true)
		if !regexp.MustCompile(`id="msg-` + id + `"(?s:.*?)flag saved`).MatchString(page) {
			t.Fatal("the channel does not show the save to its saver")
		}
		if _, page := bob.do(http.MethodGet, "/t/"+teamName+"/ch/"+town, nil, "", true); strings.Contains(page, "flag saved") {
			t.Fatal("the channel shows another person's save")
		}
		admin.hx("/m/"+town+"/"+id+"/delete", url.Values{})
		adminWS.expect(5*time.Second, `id="pins-btn-`+town+`"`)
		if list := pins(); strings.Contains(list, id) {
			t.Fatal("a deleted message stayed pinned")
		}
		if list := saved(admin); strings.Contains(list, id) {
			t.Fatal("a deleted message stayed saved")
		}
	})

	t.Run("statuses show and do not disturb holds notifications", func(t *testing.T) {
		_, list := admin.do(http.MethodGet, "/t/"+teamName+"/people/search?q="+bobName, nil, "", true)
		bobUser := pickRe.FindStringSubmatch(list)[1]
		res, _ := admin.hx("/t/"+teamName+"/dm", url.Values{"user_id": {bobUser}})
		direct := channelRe.FindStringSubmatch(res.Header.Get("HX-Location"))[1]
		setStatus := func(status, emoji, text string) (*http.Response, string) {
			return bob.hx("/status", url.Values{"status": {status}, "dnd": {"0"}, "emoji": {emoji}, "text": {text}, "clear": {"60"}})
		}
		if _, modal := bob.do(http.MethodGet, "/status", nil, "", true); !strings.Contains(modal, "Set your status") || !strings.Contains(modal, `value="dnd"`) {
			t.Fatalf("status modal: %s", modal)
		}
		if res, body := setStatus("dnd", "calendar", "In a meeting"); res.StatusCode != http.StatusOK || !strings.Contains(body, `class="presence dnd"`) || !strings.Contains(body, "In a meeting") {
			t.Fatalf("set status: %d %s", res.StatusCode, body)
		}
		adminWS.expect(5*time.Second, `id="presence-`+bobUser+`" class="presence dnd"`)
		adminWS.expect(5*time.Second, `id="channel-lists"`, `class="cs" title="In a meeting"`)
		if _, page := admin.do(http.MethodGet, "/t/"+teamName+"/ch/"+direct, nil, "", true); !strings.Contains(page, `class="custom-status"`) || !strings.Contains(page, `class="presence dnd"`) {
			t.Fatalf("the conversation does not show bob's status: %s", page)
		}
		adminWS.send(map[string]string{"op": "post", "channel_id": direct, "content": "are you there"})
		frames := strings.Join(bobWS.collect(2*time.Second), "\n")
		if strings.Contains(frames, `id="notify"`) || !strings.Contains(frames, `id="unread-`+direct+`" class="mentions"`) {
			t.Fatalf("do not disturb notified, or did not count the message: %s", frames)
		}
		if res, _ := setStatus("busy", "", ""); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("an unknown status: %d", res.StatusCode)
		}
		if res, _ := setStatus("online", "<b>", "hi"); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("an unknown emoji: %d", res.StatusCode)
		}
		adminWS.expect(5*time.Second, `id="presence-`+bobUser+`" class="presence on"`)
		setStatus("offline", "", "")
		adminWS.expect(5*time.Second, `id="presence-`+bobUser+`" class="presence "`)
		if res, body := setStatus("online", "", ""); res.StatusCode != http.StatusOK || !strings.Contains(body, "@"+bobName) {
			t.Fatalf("clear status: %d %s", res.StatusCode, body)
		}
		adminWS.expect(5*time.Second, `id="presence-`+bobUser+`" class="presence on"`)
		adminWS.send(map[string]string{"op": "post", "channel_id": direct, "content": "now?"})
		bobWS.expect(5*time.Second, `id="notify"`, "now?")
	})

	t.Run("csrf and origin are enforced", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/t/"+teamName+"/invites", nil)
		req.Header.Set("HX-Request", "true")
		res, err := admin.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("a request without the CSRF token passed: %d", res.StatusCode)
		}
		req, _ = http.NewRequest(http.MethodPost, baseURL+"/t/"+teamName+"/invites", nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("X-CSRF-Token", admin.cookie("gochat_csrf"))
		res, err = admin.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("a cross-origin request passed: %d", res.StatusCode)
		}
		if _, res, err := admin.socket("https://evil.example"); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
			t.Fatalf("a cross-origin socket was accepted")
		}
	})

	t.Run("proxies may not rewrite pages", func(t *testing.T) {
		res, body := admin.get("/settings")
		if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") || !strings.Contains(cc, "no-transform") {
			t.Fatalf("page Cache-Control: %q", cc)
		}
		if res, _ := admin.do(http.MethodGet, "/settings", nil, "", true); !strings.Contains(res.Header.Get("Cache-Control"), "no-transform") {
			t.Fatalf("fragment Cache-Control: %q", res.Header.Get("Cache-Control"))
		}
		css := staticCSSRe.FindString(body)
		if css == "" {
			t.Fatalf("no stylesheet in %s", body)
		}
		if res, _ := admin.get(css); res.StatusCode != http.StatusOK || strings.Contains(res.Header.Get("Cache-Control"), "no-transform") {
			t.Fatalf("static asset: %d %q", res.StatusCode, res.Header.Get("Cache-Control"))
		}
	})

	t.Run("members cannot invite", func(t *testing.T) {
		res, _ := bob.hx("/t/"+teamName+"/invites", url.Values{})
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("a member created an invite: %d", res.StatusCode)
		}
	})

	t.Run("invites die with their creator's rights", func(t *testing.T) {
		_, list := admin.do(http.MethodGet, "/t/"+teamName+"/members", nil, "", true)
		m := regexp.MustCompile(`@` + regexp.QuoteMeta(bobName) + `(?s:.*?)/members/([0-9a-f-]{36})/role`).FindStringSubmatch(list)
		if m == nil {
			t.Fatalf("bob is not in the member list: %s", list)
		}
		bobID := m[1]
		admin.hx("/t/"+teamName+"/members/"+bobID+"/role", url.Values{"role": {"admin"}})
		_, body := bob.hx("/t/"+teamName+"/invites", url.Values{})
		link := inviteRe.FindStringSubmatch(body)
		if link == nil {
			t.Fatalf("a team admin could not invite: %s", body)
		}
		if _, page := newClient(t, "carol").get("/join/" + link[1]); !strings.Contains(page, "Create an account") {
			t.Fatalf("a fresh invite is not usable: %s", page)
		}
		admin.hx("/t/"+teamName+"/members/"+bobID+"/role", url.Values{"role": {"member"}})
		if _, page := newClient(t, "carol").get("/join/" + link[1]); !strings.Contains(page, "Invite unavailable") {
			t.Fatalf("an invite outlived its creator's admin role: %s", page)
		}
		_, modal := admin.do(http.MethodGet, "/t/"+teamName+"/invite", nil, "", true)
		entry := "/invites/" + secure.PublicID(secure.Hash(link[1])) + "/revoke"
		if i := strings.Index(modal, entry); i < 0 || !strings.Contains(modal[:i], "Suspended") {
			t.Fatalf("the suspended invite is not shown as such: %s", modal)
		}
	})

	t.Run("a removed member cannot rejoin with their invite", func(t *testing.T) {
		_, body := admin.hx("/t/"+teamName+"/invites", url.Values{})
		link := inviteRe.FindStringSubmatch(body)
		if link == nil {
			t.Fatalf("no invite link in %s", body)
		}
		frank := newClient(t, "frank")
		name, pass := "frank-"+random(4), "pw-"+random(8)
		frank.get("/signup?invite=" + link[1])
		res, body := frank.form("/signup", url.Values{
			"username": {name}, "display_name": {"Frank"}, "email": {name + "@example.test"},
			"password": {pass}, "confirm": {pass}, "invite": {link[1]},
		})
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("signup: %d %s", res.StatusCode, body)
		}
		_, list := admin.do(http.MethodGet, "/t/"+teamName+"/members", nil, "", true)
		m := regexp.MustCompile(`@` + regexp.QuoteMeta(name) + `(?s:.*?)/members/([0-9a-f-]{36})/remove`).FindStringSubmatch(list)
		if m == nil {
			t.Fatalf("frank is not in the member list: %s", list)
		}
		admin.hx("/t/"+teamName+"/members/"+m[1]+"/remove", url.Values{})
		frank.form("/join/"+link[1], url.Values{})
		if res, _ := frank.get("/t/" + teamName); res.StatusCode == http.StatusOK {
			t.Fatal("a removed member rejoined with the invite they used")
		}
	})

	t.Run("revoked invites stop working", func(t *testing.T) {
		_, body := admin.hx("/t/"+teamName+"/invites", url.Values{})
		link := inviteRe.FindStringSubmatch(body)
		if link == nil {
			t.Fatalf("no invite link in %s", body)
		}
		revoke := "/t/" + teamName + "/invites/" + secure.PublicID(secure.Hash(link[1])) + "/revoke"
		if !strings.Contains(body, revoke) {
			t.Fatalf("the new invite is not listed: %s", body)
		}
		if i := strings.Index(body, `id="invite-list"`); i < 0 || strings.Contains(body[i:], link[1]) {
			t.Fatalf("the open invites are missing or show tokens: %s", body)
		}
		if res, _ := bob.do(http.MethodGet, "/t/"+teamName+"/invite", nil, "", true); res.StatusCode != http.StatusForbidden {
			t.Fatalf("a member listed the invites: %d", res.StatusCode)
		}
		if res, _ := bob.hx(revoke, url.Values{}); res.StatusCode != http.StatusForbidden {
			t.Fatalf("a member revoked an invite: %d", res.StatusCode)
		}
		res, list := admin.hx(revoke, url.Values{})
		if res.StatusCode != http.StatusOK || !strings.Contains(list, `id="invite-list"`) || strings.Contains(list, revoke) {
			t.Fatalf("a revoked invite is still listed: %s", list)
		}
		if _, page := newClient(t, "dave").get("/join/" + link[1]); !strings.Contains(page, "Invite unavailable") {
			t.Fatalf("a revoked invite still works: %s", page)
		}
		if _, again := admin.hx(revoke, url.Values{}); !strings.Contains(again, "invite not found") {
			t.Fatalf("revoking twice: %s", again)
		}
	})

	t.Run("profile and team photos", func(t *testing.T) {
		pic := image.NewRGBA(image.Rect(0, 0, 400, 300))
		for i := range pic.Pix {
			if pic.Pix[i] = uint8(i); i%4 == 3 {
				pic.Pix[i] = 255
			}
		}
		var raw bytes.Buffer
		_ = png.Encode(&raw, pic)
		upload := func(c *client, path string, name string, data []byte) string {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, _ := mw.CreateFormFile("photo", name)
			_, _ = fw.Write(data)
			_ = mw.Close()
			res, body := c.do(http.MethodPost, path, &buf, mw.FormDataContentType(), true)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s", path, res.StatusCode, body)
			}
			return body
		}
		body := upload(bob, "/settings/photo", "me.png", raw.Bytes())
		m := photoRe.FindStringSubmatch(body)
		if m == nil || m[1] != "u" {
			t.Fatalf("no photo after upload: %s", body)
		}
		userPhoto := m[0]
		res, got := admin.get(userPhoto)
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/jpeg" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
			t.Fatalf("photo: %d %v", res.StatusCode, res.Header)
		}
		if cfg, _, err := image.DecodeConfig(strings.NewReader(got)); err != nil || cfg.Width != 256 || cfg.Height != 256 {
			t.Fatalf("photo is not a 256px square: %v %v", cfg, err)
		}
		if res, _ := newClient(t, "anon").get(userPhoto); res.StatusCode == http.StatusOK {
			t.Fatal("a signed-out client got a photo")
		}
		if body := upload(bob, "/settings/photo", "x.svg", []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")); !strings.Contains(body, "Use a JPEG, PNG, GIF or WebP picture") {
			t.Fatalf("svg accepted: %s", body)
		}

		if body := upload(bob, "/t/"+teamName+"/photo", "team.png", raw.Bytes()); !strings.Contains(body, "form-error") || strings.Contains(body, "Saved.") {
			t.Fatalf("a member set the team photo: %s", body)
		}
		var tinted bytes.Buffer
		sq := image.NewNRGBA(image.Rect(0, 0, 64, 64))
		for i := range 64 * 64 {
			sq.Set(i%64, i/64, color.NRGBA{200, 210, 250, 120})
		}
		_ = png.Encode(&tinted, sq)
		body = upload(admin, "/t/"+teamName+"/photo", "team.png", tinted.Bytes())
		m = photoRe.FindStringSubmatch(body)
		if m == nil || m[1] != "t" || !strings.Contains(body, `id="team-rail"`) {
			t.Fatalf("team photo: %s", body)
		}
		if res, _ := bob.get(m[0]); res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("member fetching the team photo: %d %v", res.StatusCode, res.Header)
		}
		if _, body := admin.hx("/t/"+teamName+"/photo/remove", url.Values{}); photoRe.MatchString(body) {
			t.Fatalf("team photo not removed: %s", body)
		}

		if _, body := bob.hx("/settings/photo/remove", url.Values{}); photoRe.MatchString(body) {
			t.Fatalf("photo not removed: %s", body)
		}
		if res, _ := admin.get(userPhoto); res.StatusCode != http.StatusNotFound {
			t.Fatalf("removed photo still served: %d", res.StatusCode)
		}
	})

	t.Run("forms refuse multipart bodies and open redirects", func(t *testing.T) {
		res, _ := admin.do(http.MethodPost, "/t/"+teamName+"/invites", strings.NewReader("--x--"), "multipart/form-data; boundary=x", true)
		if res.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("a multipart form body was accepted: %d", res.StatusCode)
		}
		res, _ = admin.get("/login?next=" + url.QueryEscape("/\t/evil.example"))
		if loc := res.Header.Get("Location"); loc != "/" {
			t.Fatalf("redirected to %q", loc)
		}
	})

	t.Run("two-factor sign-in", func(t *testing.T) {
		_, body := bob.hx("/settings/totp/begin", url.Values{"password": {bobPass}})
		m := secretRe.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no secret: %s", body)
		}
		code, _ := totp.GenerateCode(m[1], time.Now())
		_, body = bob.hx("/settings/totp/confirm", url.Values{"code": {code}})
		codes := codeRe.FindAllStringSubmatch(body, -1)
		if len(codes) != 10 {
			t.Fatalf("no recovery codes: %s", body)
		}
		fresh := newClient(t, "bob2")
		res, _ := fresh.form("/login", url.Values{"login": {bobName}, "password": {bobPass}})
		if res.Header.Get("Location") != "/login/mfa" || fresh.cookie("gochat_session") != "" {
			t.Fatalf("password alone signed in: %d %s", res.StatusCode, res.Header.Get("Location"))
		}
		_, _ = fresh.form("/login/mfa", url.Values{"code": {code}})
		if fresh.cookie("gochat_session") != "" {
			t.Fatal("a used TOTP code was accepted again")
		}
		res, _ = fresh.form("/login/mfa", url.Values{"code": {codes[0][1]}})
		if res.StatusCode != http.StatusSeeOther || fresh.cookie("gochat_session") == "" {
			t.Fatalf("recovery code refused: %d", res.StatusCode)
		}
		again := newClient(t, "bob3")
		again.form("/login", url.Values{"login": {bobName}, "password": {bobPass}})
		again.form("/login/mfa", url.Values{"code": {codes[0][1]}})
		if again.cookie("gochat_session") != "" {
			t.Fatal("a recovery code worked twice")
		}
	})

	// live opens c's socket and waits until it carries c's own posts.
	live := func(t *testing.T, c *client) *socket {
		t.Helper()
		ws, _, err := c.socket(baseURL)
		if err != nil {
			t.Fatal(err)
		}
		ws.t = t
		view(ws, town)
		word := "here-" + random(4)
		ws.send(map[string]string{"op": "post", "channel_id": town, "content": word})
		ws.expect(5*time.Second, word)
		return ws
	}

	t.Run("deactivation signs out at once and bars sign-in", func(t *testing.T) {
		dave, daveName, davePass := join(t, admin, "dave")
		ws := live(t, dave)
		daveID := adminFind(t, admin, daveName)
		deactivate := func(c *client, on string) (*http.Response, string) {
			return c.hx("/admin/users/"+daveID+"/deactivate", url.Values{"deactivated": {on}})
		}
		if res, _ := deactivate(bob, "1"); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a member deactivated someone: %d", res.StatusCode)
		}
		if res, _ := bob.get("/admin/users?q=" + daveName); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a member opened the admin page: %d", res.StatusCode)
		}
		res, card := deactivate(admin, "1")
		if res.StatusCode != http.StatusOK || !strings.Contains(card, "Reactivate") || !strings.Contains(card, "Not signed in anywhere") {
			t.Fatalf("deactivate: %d %s", res.StatusCode, card)
		}
		ws.expect(5*time.Second, `data-redirect="/login"`)
		ws.closed(5 * time.Second)
		if !signedOut(dave) {
			t.Fatal("a deactivated user's session still works")
		}
		if again, _, err := dave.socket(baseURL); err == nil {
			again.t = t
			again.expect(5*time.Second, `data-redirect="/login"`)
		}
		fresh := newClient(t, "dave2")
		if res, body := fresh.form("/login", url.Values{"login": {daveName}, "password": {davePass}}); res.StatusCode == http.StatusSeeOther || !strings.Contains(body, "account is deactivated") {
			t.Fatalf("a deactivated user signed in: %d %s", res.StatusCode, body)
		}
		if _, card := deactivate(admin, "0"); !strings.Contains(card, `"deactivated": "1"`) {
			t.Fatalf("reactivate: %s", card)
		}
		fresh.login(daveName, davePass)
		if _, card := admin.hx("/admin/users/"+adminFind(t, admin, adminName)+"/deactivate", url.Values{"deactivated": {"1"}}); !strings.Contains(card, "cannot deactivate your own account") {
			t.Fatalf("the admin deactivated themself: %s", card)
		}
	})

	t.Run("people see and revoke their sessions", func(t *testing.T) {
		erin, erinName, erinPass := join(t, admin, "erin")
		laptop := newClient(t, "erin-laptop")
		laptop.login(erinName, erinPass)
		laptopWS := live(t, laptop)
		_, page := erin.get("/settings")
		others := sessionRe.FindAllStringSubmatch(page, -1)
		if len(others) != 1 || !strings.Contains(page, "This device") || !strings.Contains(page, "Go-http-client") {
			t.Fatalf("sessions on the settings page: %s", page)
		}
		res, list := erin.hx("/settings/sessions/"+others[0][1]+"/revoke", url.Values{})
		if res.StatusCode != http.StatusOK || strings.Contains(list, others[0][1]) || !strings.Contains(list, "Signed out.") {
			t.Fatalf("revoke a session: %d %s", res.StatusCode, list)
		}
		laptopWS.expect(5*time.Second, `data-redirect="/login"`)
		if !signedOut(laptop) || signedOut(erin) {
			t.Fatal("the wrong session was signed out")
		}
		if _, list := erin.hx("/settings/sessions/"+strings.Repeat("0", 24)+"/revoke", url.Values{}); !strings.Contains(list, "session not found") {
			t.Fatalf("revoking a session that is not hers: %s", list)
		}

		phone := newClient(t, "erin-phone")
		phone.login(erinName, erinPass)
		if _, list := erin.hx("/settings/sessions/revoke", url.Values{}); !strings.Contains(list, "Every other session was signed out.") {
			t.Fatalf("sign out the others: %s", list)
		}
		if !signedOut(phone) || signedOut(erin) {
			t.Fatal("signing out the others did not keep only this one")
		}

		erinWS := live(t, erin)
		erinID := adminFind(t, admin, erinName)
		if res, _ := bob.hx("/admin/users/"+erinID+"/sessions/revoke", url.Values{}); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a member signed someone out: %d", res.StatusCode)
		}
		res, card := admin.hx("/admin/users/"+erinID+"/sessions/revoke", url.Values{})
		if res.StatusCode != http.StatusOK || !strings.Contains(card, "Signed out everywhere.") || !strings.Contains(card, "Not signed in anywhere") {
			t.Fatalf("admin sign-out: %d %s", res.StatusCode, card)
		}
		erinWS.expect(5*time.Second, `data-redirect="/login"`)
		if !signedOut(erin) {
			t.Fatal("erin's session survived the admin's sign-out")
		}
	})

	t.Run("storage quotas bound uploads", func(t *testing.T) {
		fay, fayName, _ := join(t, admin, "fay")
		fayID := adminFind(t, admin, fayName)
		quota := func(c *client, v url.Values) (*http.Response, string) {
			return c.hx("/admin/users/"+fayID+"/quota", v)
		}
		if res, _ := quota(bob, url.Values{"mib": {"100"}}); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a member set a quota: %d", res.StatusCode)
		}
		if res, card := quota(admin, url.Values{"mib": {"1"}}); res.StatusCode != http.StatusOK || !strings.Contains(card, "Quota saved.") || !strings.Contains(card, "0 B used of 1.0 MB") {
			t.Fatalf("set a quota: %d %s", res.StatusCode, card)
		}
		upload := func(size int) (*http.Response, string) {
			data := make([]byte, size)
			_, _ = rand.Read(data)
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, _ := mw.CreateFormFile("file", "data.bin")
			_, _ = fw.Write(data)
			_ = mw.Close()
			return fay.do(http.MethodPost, "/upload?channel="+town, &buf, mw.FormDataContentType(), true)
		}
		res, body := upload(700 << 10)
		m := attRe.FindStringSubmatch(body)
		if res.StatusCode != http.StatusOK || m == nil {
			t.Fatalf("an upload within the quota: %d %s", res.StatusCode, body)
		}
		if _, page := fay.get("/settings"); !strings.Contains(page, "700.0 KB used of 1.0 MB") {
			t.Fatalf("usage on the settings page: %s", page)
		}
		if res, _ := upload(700 << 10); res.StatusCode == http.StatusOK || !strings.Contains(res.Header.Get("X-Error"), "not enough storage left") {
			t.Fatalf("an upload passed the quota: %d %q", res.StatusCode, res.Header.Get("X-Error"))
		}

		ws := live(t, fay)
		ws.send(map[string]any{"op": "post", "channel_id": town, "content": "big file", "attachment": m[1]})
		msg := msgIDRe.FindStringSubmatch(ws.expect(5*time.Second, "/files/"+m[1]))[1]
		if res, _ := fay.hx("/m/"+town+"/"+msg+"/delete", url.Values{}); res.StatusCode != http.StatusOK {
			t.Fatalf("delete the message: %d", res.StatusCode)
		}
		for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(300 * time.Millisecond) {
			if _, page := fay.get("/settings"); strings.Contains(page, "0 B used of 1.0 MB") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("deleting the message did not free its space")
			}
		}
		if res, body := upload(700 << 10); res.StatusCode != http.StatusOK {
			t.Fatalf("an upload after freeing space: %d %s", res.StatusCode, body)
		}
		if _, card := quota(admin, url.Values{"default": {"1"}}); strings.Contains(card, "of 1.0 MB") || !strings.Contains(card, "Quota saved.") {
			t.Fatalf("back to the default quota: %s", card)
		}
	})

	t.Run("logout revokes the socket", func(t *testing.T) {
		res, _ := bob.hx("/logout", url.Values{})
		if res.Header.Get("HX-Redirect") != "/login" {
			t.Fatalf("logout: %d %v", res.StatusCode, res.Header)
		}
		bobWS.expect(5*time.Second, `data-redirect="/login"`)
	})

	fmt.Fprintln(os.Stderr, "e2e users:", bobName)
}
