package unfurl

import (
	"bytes"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// The longest texts a preview keeps, in characters.
const (
	maxTitle       = 200
	maxDescription = 400
	maxSiteName    = 80
)

// Page is what a page says of itself.
type Page struct {
	Title       string
	Description string
	SiteName    string
	// Image is the absolute http(s) URL of its picture.
	Image string
}

// Empty reports whether the page says nothing worth a preview.
func (p Page) Empty() bool { return p.Title == "" && p.Description == "" }

// Parse reads the Open Graph and Twitter card tags of an HTML page, falling
// back to its <title> and meta description, and to its host for the site's
// name. base is where the page was fetched from; contentType its header,
// for the charset.
func Parse(body []byte, contentType string, base *url.URL) Page {
	var r io.Reader = bytes.NewReader(body)
	// Unless the header or a byte order mark says otherwise, a page that
	// reads as UTF-8 is: the guess from its first bytes may be wrong.
	if enc, _, certain := charset.DetermineEncoding(body, contentType); certain || !validUTF8(body) {
		r = enc.NewDecoder().Reader(r)
	}
	meta := map[string]string{}
	var title string
	z := html.NewTokenizer(r)
	inTitle, svg := false, 0
scan:
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			break scan
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch atom.Lookup(name) {
			case atom.Svg:
				if tt == html.StartTagToken {
					svg++
				}
			case atom.Title:
				inTitle = tt == html.StartTagToken && svg == 0 && title == ""
			case atom.Meta:
				if hasAttr {
					key, value := metaOf(z)
					if _, seen := meta[key]; key != "" && !seen {
						meta[key] = value
					}
				}
			case atom.Body:
				// The tags belong in the head; a body is past them.
				if len(meta) > 0 || title != "" {
					break scan
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch atom.Lookup(name) {
			case atom.Svg:
				svg = max(svg-1, 0)
			case atom.Title:
				inTitle = false
			case atom.Head:
				break scan
			}
		case html.TextToken:
			if inTitle {
				title += string(z.Text())
			}
		}
	}
	p := Page{
		Title:       clean(first(meta["og:title"], meta["twitter:title"], title), maxTitle),
		Description: clean(first(meta["og:description"], meta["twitter:description"], meta["description"]), maxDescription),
		SiteName:    clean(meta["og:site_name"], maxSiteName),
	}
	if p.SiteName == "" && base != nil {
		p.SiteName = strings.TrimPrefix(base.Hostname(), "www.")
	}
	for _, raw := range []string{meta["og:image:secure_url"], meta["og:image"], meta["og:image:url"], meta["twitter:image"], meta["twitter:image:src"]} {
		if img := absolute(base, raw); img != "" {
			p.Image = img
			break
		}
	}
	return p
}

// validUTF8 reports whether b is UTF-8, but for a rune cut at its end.
func validUTF8(b []byte) bool {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				b = b[:len(b)-i]
			}
			break
		}
	}
	return utf8.Valid(b)
}

// metaOf reads a <meta> tag's key, its property or name, and its content.
func metaOf(z *html.Tokenizer) (string, string) {
	var property, name, content string
	for {
		k, v, more := z.TagAttr()
		switch string(k) {
		case "property":
			property = string(v)
		case "name":
			name = string(v)
		case "content":
			content = string(v)
		}
		if !more {
			break
		}
	}
	key := property
	if key == "" {
		key = name
	}
	return strings.ToLower(strings.TrimSpace(key)), content
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// clean folds whitespace, drops control characters and cuts s to at most
// n characters, with an ellipsis where it cut.
func clean(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r) || r == utf8.RuneError || unicode.Is(unicode.Cf, r):
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) <= n {
		return out
	}
	cut := []rune(out)[:n-1]
	return strings.TrimRightFunc(string(cut), unicode.IsSpace) + "…"
}

// absolute resolves a picture's URL against the page's, keeping http(s).
func absolute(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > MaxURL {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

// IsHTML takes the media types of pages.
func IsHTML(media string) bool {
	return media == "text/html" || media == "application/xhtml+xml"
}

// IsImage takes the media types pictures come in; the bytes decide.
func IsImage(media string) bool {
	return strings.HasPrefix(media, "image/") || media == "" || media == "application/octet-stream"
}
