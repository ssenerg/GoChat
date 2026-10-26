package render

import (
	"bytes"
	"strings"
	"testing"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
)

func TestTemplatesParse(t *testing.T) {
	if _, err := New(16); err != nil {
		t.Fatal(err)
	}
}

func TestMarkdownIsSafe(t *testing.T) {
	r, err := New(16)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"<script>alert(1)</script>":          "<script",
		"[x](javascript:alert(1))":           "javascript:",
		"![t](https://evil.example/t.png)":   "<img",
		"<img src=x onerror=alert(1)>":       "onerror",
		"<a href=\"https://x\">x</a>":        "<a href=\"https://x\">",
		"[x](https://ok.example \"t\")":      "<script",
		"hi <b onclick=\"x()\">there</b>":    "onclick",
		"`<i>` and ```\n<svg onload=1>\n```": "<svg",
	}
	for in, bad := range cases {
		out := string(r.markdown(in))
		if strings.Contains(out, bad) {
			t.Errorf("%q rendered %q", in, out)
		}
	}
	out := string(r.markdown("see https://example.com"))
	if !strings.Contains(out, `rel="nofollow noopener noreferrer"`) || !strings.Contains(out, `target="_blank"`) {
		t.Errorf("autolink lacks rel and target: %q", out)
	}
}

func TestMarkdownCannotBlowUp(t *testing.T) {
	r, err := New(16)
	if err != nil {
		t.Fatal(err)
	}
	wide := strings.Repeat("|a", 2000) + "|\n" + strings.Repeat("|-", 2000) + "|\n" + strings.Repeat("|\n", 4000)
	long := "|a|b|\n|-|-|\n" + strings.Repeat("|\n", 7000)
	cases := []string{wide, long, strings.Repeat(">", 8000) + " x", strings.Repeat("- ", 5000) + "x"}
	for _, in := range cases {
		if out := r.markdown(in); len(out) > 4*len(in)+64 {
			t.Errorf("%d bytes of input rendered to %d", len(in), len(out))
		}
	}
	if out := string(r.markdown("|a|b|\n|-|-|\n|1|2|")); !strings.Contains(out, "<table>") {
		t.Errorf("small table not rendered: %q", out)
	}
	if out := string(r.markdown("> quote\n- item\n  - nested")); !strings.Contains(out, "<blockquote>") || !strings.Contains(out, "<ul>") {
		t.Errorf("quote and list not rendered: %q", out)
	}
}

func TestUnread(t *testing.T) {
	older := "00000000-0000-1000-8000-000000000000"
	newer := "00000001-0000-1000-8000-000000000000"
	cross := "ffffffff-0000-1000-8000-000000000000"
	later := "00000000-0001-1000-8000-000000000000"
	if !Unread(&pb.Channel{LastMessageId: newer, LastReadId: older}) {
		t.Error("newer message should be unread")
	}
	if Unread(&pb.Channel{LastMessageId: older, LastReadId: newer}) {
		t.Error("older message should be read")
	}
	if !Unread(&pb.Channel{LastMessageId: later, LastReadId: cross}) {
		t.Error("time_mid outranks time_low")
	}
}

func TestMessageFragment(t *testing.T) {
	r, err := New(16)
	if err != nil {
		t.Fatal(err)
	}
	m := r.Message(&pb.Message{Id: "id1", ChannelId: "c1", Content: "<b>hi</b>", Author: &pb.User{Id: "u", Username: "u", DisplayName: "<U>"}}, &pb.User{Id: "u", Username: "u"}, false)
	var buf bytes.Buffer
	if err := r.Fragment(&buf, "message-new", m); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "<b>hi") || strings.Contains(out, "<U>") {
		t.Errorf("unescaped content: %s", out)
	}
	if !strings.Contains(out, `hx-swap-oob="afterbegin"`) {
		t.Errorf("missing oob: %s", out)
	}
}

func TestPhotos(t *testing.T) {
	r, err := New(16)
	if err != nil {
		t.Fatal(err)
	}
	u := &pb.User{Id: "11111111-1111-4111-8111-111111111111", Username: "ann", Photo: "0123456789abcdef"}
	var out bytes.Buffer
	field := map[string]any{"ID": "profile-photo", "URL": "/settings/photo", "User": u}
	if err := r.Fragment(&out, "photo-field", field); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, `src="/photos/u/`+u.Id+`/0123456789abcdef"`) || !strings.Contains(s, "Remove") {
		t.Fatalf("user photo field: %s", s)
	}
	out.Reset()
	team := &pb.Team{Id: "22222222-2222-4222-8222-222222222222", Name: "acme", DisplayName: "Acme"}
	if err := r.Fragment(&out, "photo-field", struct {
		ID, URL, Error, Status string
		User                   *pb.User
		Team                   *pb.Team
	}{ID: "team-photo", URL: "/t/acme/photo", Team: team}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Contains(s, "<img") || strings.Contains(s, "Remove") || !strings.Contains(s, `lg" aria-hidden="true">A</span>`) {
		t.Fatalf("team without photo: %s", s)
	}
	out.Reset()
	if err := r.Fragment(&out, "avatar", map[string]any{"User": (*pb.User)(nil), "Size": "sm"}); err != nil {
		t.Fatalf("nil user: %v", err)
	}
}

func TestMentions(t *testing.T) {
	r, err := New(16)
	if err != nil {
		t.Fatal(err)
	}
	body := string(r.markdown("hey @Bob and bob@example.com, `@code`"))
	if !strings.Contains(body, `<span class="mention">@bob</span>`) || strings.Contains(body, `">@example`) || strings.Contains(body, `">@code`) {
		t.Fatalf("mention spans: %s", body)
	}
	bob := &pb.User{Id: "b", Username: "bob"}
	if !r.Message(&pb.Message{Id: "1", Content: "hi @bob", Author: &pb.User{Id: "a"}}, bob, false).Mentioned {
		t.Error("a mention of bob is not marked for bob")
	}
	if r.Message(&pb.Message{Id: "2", Content: "hi @bob", Author: bob}, bob, false).Mentioned {
		t.Error("bob's own message mentions him")
	}
	if !r.Message(&pb.Message{Id: "3", Content: "@here standup", Author: &pb.User{Id: "a"}}, bob, false).Mentioned {
		t.Error("@here does not mention bob")
	}
}

func TestReactions(t *testing.T) {
	r, err := New(16)
	if err != nil {
		t.Fatal(err)
	}
	if body := string(r.markdown("ship it :rocket: at 10:30:45 and :nope:")); !strings.Contains(body, `title=":rocket:">🚀</span>`) || strings.Contains(body, ":nope:\">") {
		t.Fatalf("shortcodes: %s", body)
	}
	me := &pb.User{Id: "me"}
	v := Reactions("c", "m", "", []*pb.Reaction{{Emoji: "tada", UserIds: []string{"x", "me"}}, {Emoji: "bogus", UserIds: []string{"x"}}}, me, false)
	if len(v.List) != 1 || !v.List[0].Mine || v.List[0].Count != 2 || v.List[0].Char != "🎉" {
		t.Fatalf("reaction view: %+v", v)
	}
	var buf bytes.Buffer
	if err := r.Fragment(&buf, "reactions", v); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, `"remove": "1"`) || !strings.Contains(out, `aria-pressed="true"`) {
		t.Fatalf("reaction bar: %s", out)
	}
	buf.Reset()
	if err := r.Fragment(&buf, "reactions", Reactions("c", "m", "", nil, me, false)); err != nil || buf.String() != `<div class="reactions" id="reactions-m"></div>` {
		t.Fatalf("empty bar: %q %v", buf.String(), err)
	}
}

func TestHighlight(t *testing.T) {
	got := string(Highlight("Deploying <b>now</b>,\n then deploy & rest", []string{"deploy"}))
	want := "<mark>Deploying</mark> &lt;b&gt;now&lt;/b&gt;, then <mark>deploy</mark> &amp; rest"
	if got != want {
		t.Fatalf("Highlight = %q, want %q", got, want)
	}
	long := strings.Repeat("filler words here ", 40) + "the needle sits here " + strings.Repeat("and more after ", 40)
	got = string(Highlight(long, []string{"needl"}))
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || !strings.Contains(got, "<mark>needle</mark>") {
		t.Fatalf("long Highlight = %q", got)
	}
}

func TestBotMessageShowsItsButtons(t *testing.T) {
	r, err := New(10)
	if err != nil {
		t.Fatal(err)
	}
	bot := &pb.User{Id: "b", Username: "helper_bot", DisplayName: "Helper", Bot: true}
	m := r.Message(&pb.Message{Id: "id1", ChannelId: "c1", Content: "Agree?", Author: bot, Markup: &pb.Markup{Rows: []*pb.ButtonRow{{Buttons: []*pb.Button{
		{Text: "Yes <b>", Action: &pb.Button_CallbackData{CallbackData: `y"es`}},
		{Text: "Docs", Action: &pb.Button_Url{Url: "https://example.com/docs"}},
		{Text: "Bad", Action: &pb.Button_Url{Url: "javascript:alert(1)"}},
	}}}}}, &pb.User{Id: "u", Username: "u"}, false)
	b, err := r.MessageBytes("message", m)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{`class="badge bot"`, `data-press="y&#34;es"`, `data-message="id1"`, `Yes &lt;b&gt;`, `href="https://example.com/docs"`, `rel="nofollow noopener noreferrer"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, "javascript:") {
		t.Error("a javascript: link was rendered")
	}
}
