package unfurl

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

// MaxURL bounds the links previewed.
const MaxURL = 2048

// The parser reads links as the web renders messages: Markdown links,
// <autolinks> and bare URLs, none inside code.
var md = goldmark.New(goldmark.WithExtensions(extension.Linkify))

// FirstLink returns the first http or https link of a message, empty when
// it has none.
func FirstLink(content string) string {
	if !strings.Contains(content, "http") && !strings.Contains(content, "www.") {
		return ""
	}
	src := []byte(content)
	var found string
	_ = ast.Walk(md.Parser().Parse(text.NewReader(src)), func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var dest string
		switch v := n.(type) {
		case *ast.Link:
			dest = string(v.Destination)
		case *ast.Image:
			dest = string(v.Destination)
		case *ast.AutoLink:
			if v.AutoLinkType == ast.AutoLinkURL {
				dest = string(v.URL(src))
			}
		default:
			return ast.WalkContinue, nil
		}
		lower := strings.ToLower(dest)
		if len(dest) <= MaxURL && (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) {
			found = dest
			return ast.WalkStop, nil
		}
		return ast.WalkSkipChildren, nil
	})
	return found
}

// Hosts allows and denies host names. A name covers its subdomains: deny
// example.com and www.example.com is denied too.
type Hosts struct {
	// Allow, when not empty, is all that may be previewed.
	Allow []string
	Deny  []string
}

// Allows reports whether pages of host may be previewed.
func (h Hosts) Allows(host string) bool {
	return !covered(h.Deny, host) && (len(h.Allow) == 0 || covered(h.Allow, host))
}

// Denies reports whether host is denied, for the pictures of pages, which
// sites keep on hosts of their own.
func (h Hosts) Denies(host string) bool { return covered(h.Deny, host) }

func covered(list []string, host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, name := range list {
		name = strings.TrimSuffix(strings.ToLower(name), ".")
		if name != "" && (host == name || strings.HasSuffix(host, "."+name)) {
			return true
		}
	}
	return false
}
