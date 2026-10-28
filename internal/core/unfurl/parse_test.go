package unfurl

import (
	"net/url"
	"strings"
	"testing"
)

func TestFirstLink(t *testing.T) {
	for content, want := range map[string]string{
		"see https://example.com/a.":                                "https://example.com/a",
		"no links here":                                             "",
		"`https://code.example/x` then http://after.example":        "http://after.example",
		"```\nhttps://block.example\n```\nhttps://out.example/p?q=1": "https://out.example/p?q=1",
		"[docs](https://md.example/x) and https://second.example":   "https://md.example/x",
		"<https://auto.example/y>":                                  "https://auto.example/y",
		"www.example.com/z":                                         "http://www.example.com/z",
		"ftp://files.example mailto:a@b.example a@b.example":        "",
		"[bad](javascript:alert(1)) https://good.example":           "https://good.example",
		"(see https://paren.example/a)":                             "https://paren.example/a",
	} {
		if got := FirstLink(content); got != want {
			t.Errorf("FirstLink(%q) = %q, want %q", content, got, want)
		}
	}
}

func parsed(t *testing.T, body, ctype, base string) Page {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return Parse([]byte(body), ctype, u)
}

func TestParseOpenGraph(t *testing.T) {
	p := parsed(t, `<!doctype html><html><head>
<title>Fallback</title>
<meta name="description" content="Fallback description">
<meta property="og:title" content="  The   &amp; Title ">
<meta property="og:description" content="What it is.">
<meta property="og:site_name" content="Example">
<meta property="og:image" content="/img/card.png">
<meta property="og:title" content="A second title">
</head><body><p>text</p></body></html>`, "text/html; charset=utf-8", "https://www.example.com/post/1")
	want := Page{Title: "The & Title", Description: "What it is.", SiteName: "Example", Image: "https://www.example.com/img/card.png"}
	if p != want {
		t.Fatalf("got %+v", p)
	}
}

func TestParseTwitterCard(t *testing.T) {
	p := parsed(t, `<head>
<meta name="twitter:title" content="Tweet title">
<meta name="twitter:description" content="Tweet text">
<meta name="twitter:image" content="https://cdn.example/t.jpg">
<title>Page</title></head>`, "text/html", "https://www.example.org/")
	want := Page{Title: "Tweet title", Description: "Tweet text", SiteName: "example.org", Image: "https://cdn.example/t.jpg"}
	if p != want {
		t.Fatalf("got %+v", p)
	}
}

func TestParseFallsBack(t *testing.T) {
	p := parsed(t, `<html><head><title>
  Plain page
</title><meta name="Description" content="A plain description."><meta property="og:image" content="javascript:alert(1)"></head>
<body><svg><title>Not this</title></svg></body></html>`, "text/html", "http://plain.example/")
	want := Page{Title: "Plain page", Description: "A plain description.", SiteName: "plain.example"}
	if p != want {
		t.Fatalf("got %+v", p)
	}
	if !parsed(t, `<html><body>nothing</body></html>`, "text/html", "http://x.example/").Empty() {
		t.Fatal("a page with nothing to say")
	}
	if p := parsed(t, `<svg><title>Icon</title></svg><title>Real</title>`, "text/html", "http://x.example/"); p.Title != "Real" {
		t.Fatalf("an svg's title: %+v", p)
	}
}

func TestParseCharset(t *testing.T) {
	// "Café" in ISO-8859-1, named by the header or by the page.
	latin := "<title>Caf\xe9</title>"
	if p := parsed(t, latin, "text/html; charset=iso-8859-1", "http://x.example/"); p.Title != "Café" {
		t.Fatalf("from the header: %q", p.Title)
	}
	if p := parsed(t, `<meta charset="iso-8859-1">`+latin, "text/html", "http://x.example/"); p.Title != "Café" {
		t.Fatalf("from the page: %q", p.Title)
	}
}

func TestParseCuts(t *testing.T) {
	long := strings.Repeat("word ", 200)
	p := parsed(t, `<meta property="og:title" content="`+long+`"><meta property="og:description" content="a`+"\u0000‮"+`b">`, "text/html", "http://x.example/")
	if n := len([]rune(p.Title)); n != maxTitle || !strings.HasSuffix(p.Title, "…") {
		t.Fatalf("title of %d characters: %q", n, p.Title)
	}
	if p.Description != "ab" {
		t.Fatalf("control characters kept: %q", p.Description)
	}
}

func TestParseCutUTF8(t *testing.T) {
	// UTF-8 past the first kilobyte, the page cut in the middle of a rune.
	body := `<meta property="og:title" content="` + strings.Repeat("x ", 600) + `">` +
		`<meta name="description" content="Café"><p>é`
	p := parsed(t, body[:len(body)-1], "text/html", "http://x.example/")
	if p.Description != "Café" {
		t.Fatalf("description %q", p.Description)
	}
}
