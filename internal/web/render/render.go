// Package render turns the chat's data into HTML: full pages, the fragments
// htmx swaps in, and the out-of-band fragments pushed over the websocket.
package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/cache"
	"github.com/ssenerg/gochat/internal/web/views"
)

type Renderer struct {
	pages     map[string]*template.Template
	fragments *template.Template
	md        goldmark.Markdown
	bodies    *cache.Cache[string, template.HTML]
	// events holds messages rendered for the sockets of this gateway, which
	// each receive the same few variants of a new or edited message.
	events *cache.Cache[string, []byte]
	// StaticPrefix is where the embedded assets are served, versioned by
	// their content so they can be cached forever.
	StaticPrefix string
}

func New(cacheSize int) (*Renderer, error) {
	r := &Renderer{
		pages:  make(map[string]*template.Template),
		md:     newMarkdown(),
		bodies: cache.New[string, template.HTML](cacheSize, time.Hour),
		events: cache.New[string, []byte](512, time.Minute),
	}
	sum, err := staticHash()
	if err != nil {
		return nil, err
	}
	r.StaticPrefix = "/static/" + sum + "/"

	base := template.New("").Funcs(r.funcs())
	base, err = base.ParseFS(views.Files, "templates/partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse partials: %w", err)
	}
	r.fragments = base
	pages, err := fs.Glob(views.Files, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := t.ParseFS(views.Files, "templates/layout.html", p); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", p, err)
		}
		name := strings.TrimSuffix(p[strings.LastIndexByte(p, '/')+1:], ".html")
		r.pages[name] = t
	}
	return r, nil
}

func staticHash() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(views.Files, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(views.Files, path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// Page renders a full page.
func (r *Renderer) Page(w io.Writer, name string, data any) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("unknown page %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

// Fragment renders one named partial.
func (r *Renderer) Fragment(w io.Writer, name string, data any) error {
	var buf bytes.Buffer
	if err := r.fragments.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

// MessageBytes renders a message fragment once per message, edit and
// variant, for every socket that shows it. The bytes are shared: read only.
func (r *Renderer) MessageBytes(name string, m *Message) ([]byte, error) {
	key := fmt.Sprintf("%s/%s/%s/%t/%t/%t/%t/%t/%t/%d/%d/%s/%s", name, m.Id, m.GetEditedAt().AsTime().Format(time.RFC3339Nano), m.Mine, m.CanDelete, m.Mentioned, m.OOB,
		m.Pinned, m.Saved, m.ReplyCount, m.GetLastReplyAt().AsTime().UnixNano(), m.bar.key(), previewKey(m.GetLinkPreview()))
	if b, ok := r.events.Get(key); ok {
		return b, nil
	}
	b, err := r.FragmentBytes(name, m)
	if err != nil {
		return nil, err
	}
	r.events.Set(key, b)
	return b, nil
}

// previewKey tells link previews apart: one comes and goes without an edit.
func previewKey(p *pb.LinkPreview) string {
	if p == nil {
		return ""
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(p)
	h := fnv.New64a()
	_, _ = h.Write(b)
	return strconv.FormatUint(h.Sum64(), 36)
}

// FragmentBytes renders a partial into a new buffer.
func (r *Renderer) FragmentBytes(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := r.fragments.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (r *Renderer) funcs() template.FuncMap {
	return template.FuncMap{
		"static":   func(name string) string { return r.StaticPrefix + name },
		"initials": Initials,
		"teamInitials": func(t *pb.Team) string {
			if t == nil {
				return "?"
			}
			return Initials(&pb.User{DisplayName: t.DisplayName})
		},
		"avatar":  AvatarClass,
		"icon":    Icon,
		"size":    HumanSize,
		"device":  Device,
		"mib":     MiB,
		"clockMS": ClockMS,
		"isImage": IsImage,
		// A wide picture goes under a preview's text, any other beside it.
		"wideImage": func(p *pb.LinkPreview) bool {
			return p.GetImageId() != "" && p.GetImageWidth() >= 320 && p.GetImageWidth()*2 >= p.GetImageHeight()*3
		},
		"iso":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"clock": func(t time.Time) string { return t.UTC().Format("15:04") },
		"date":  func(t time.Time) string { return t.UTC().Format("Jan 2, 2006") },
		"name":  DisplayName,
		"channelName": func(ch *pb.Channel) string {
			if ch == nil {
				return ""
			}
			return Title(ch)
		},
		"unread": Unread,
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict needs key and value pairs")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, errors.New("dict keys are strings")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		"isPrivate":      func(ch *pb.Channel) bool { return ch != nil && ch.Type == pb.ChannelType_CHANNEL_TYPE_PRIVATE },
		"isDirect":       func(ch *pb.Channel) bool { return ch != nil && ch.Type == pb.ChannelType_CHANNEL_TYPE_DIRECT },
		"isVoice":        func(ch *pb.Channel) bool { return ch != nil && ch.Type == pb.ChannelType_CHANNEL_TYPE_VOICE },
		"isGroup":        func(ch *pb.Channel) bool { return ch != nil && ch.Type == pb.ChannelType_CHANNEL_TYPE_GROUP },
		"isConversation": Conversation,
		"inc":            func(n int) int { return n + 1 },
		"statusClass":    StatusClass,
		"statusLabel":    StatusLabel,
		"chosen":         Chosen,
		"emojiChar":      EmojiChar,
		"isAdmin":        func(t *pb.Team) bool { return t != nil && t.Role == pb.TeamRole_TEAM_ROLE_ADMIN },
		"isSystemAdmin": func(u *pb.User) bool {
			return u != nil && u.Role == pb.SystemRole_SYSTEM_ROLE_ADMIN
		},
		"channelAdmin": func(ch *pb.Channel) bool { return ch != nil && ch.Role == pb.ChannelRole_CHANNEL_ROLE_ADMIN },
	}
}

func DisplayName(u *pb.User) string {
	if u == nil {
		return "Unknown"
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func Initials(u *pb.User) string {
	name := DisplayName(u)
	var out []rune
	for _, word := range strings.Fields(name) {
		r, _ := utf8.DecodeRuneInString(word)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, unicode.ToUpper(r))
		}
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// AvatarClass picks one of eight colors from the user's id.
func AvatarClass(u *pb.User) string {
	if u == nil {
		return "av0"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(u.Id))
	return fmt.Sprintf("av%d", h.Sum32()%8)
}

func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// MiB is n bytes in whole mebibytes, at least one, or empty for zero.
func MiB(n int64) string {
	if n <= 0 {
		return ""
	}
	return strconv.FormatInt(max(1, n>>20), 10)
}

// ClockMS shows a length in milliseconds as minutes and seconds, 1:05.
func ClockMS(ms uint32) string {
	s := (ms + 500) / 1000
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// IsImage reports whether a file may be shown inline: raster formats only,
// never SVG, which can carry script.
func IsImage(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// Unread reports whether a channel holds a message newer than the last one
// read. Time UUIDs order by time, which their string form does not, so the
// caller passes the parsed timestamps.
func Unread(ch *pb.Channel) bool {
	if ch == nil || ch.LastMessageId == "" {
		return false
	}
	if ch.LastReadId == "" {
		return true
	}
	return uuidTime(ch.LastMessageId) > uuidTime(ch.LastReadId)
}

// Newer reports whether time UUID a is later than b; anything is later than
// an empty b.
func Newer(a, b string) bool {
	return uuidTime(a) > uuidTime(b)
}

// uuidTime extracts the 60-bit timestamp of a version 1 UUID string.
func uuidTime(s string) uint64 {
	if len(s) != 36 {
		return 0
	}
	hex := s[15:18] + s[9:13] + s[0:8]
	var t uint64
	for _, c := range hex {
		t <<= 4
		switch {
		case c >= '0' && c <= '9':
			t |= uint64(c - '0')
		case c >= 'a' && c <= 'f':
			t |= uint64(c-'a') + 10
		default:
			return 0
		}
	}
	return t
}

const maxCachedBody = 16 << 10

// Body renders a message's content, cached by message and edit.
func (r *Renderer) Body(m *pb.Message) template.HTML {
	key := m.Id + "/" + m.GetEditedAt().AsTime().Format(time.RFC3339Nano)
	if h, ok := r.bodies.Get(key); ok {
		return h
	}
	h := r.markdown(m.Content)
	// The cache counts entries, so large bodies are rendered each time.
	if len(h) <= maxCachedBody {
		r.bodies.Set(key, h)
	}
	return h
}
