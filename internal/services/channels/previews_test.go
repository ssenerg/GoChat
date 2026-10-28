package channels

import (
	"context"
	"sync"
	"testing"
	"time"

	stringadapter "github.com/casbin/casbin/v2/persist/string-adapter"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// writes stores what post, edit and the previews write.
type writes struct {
	*withLog
	repo *messageRepo
}

func (w *writes) InsertMessage(_ context.Context, m *models.Message, _ bool) error {
	w.repo.mu.Lock()
	defer w.repo.mu.Unlock()
	c := *m
	w.repo.messages[m.ID] = &c
	return nil
}

func (w *writes) UpdateMessageContent(_ context.Context, _, id gocql.UUID, content string, at time.Time) error {
	w.repo.mu.Lock()
	defer w.repo.mu.Unlock()
	w.repo.messages[id].Content, w.repo.messages[id].EditedAt = content, at
	return nil
}

func (w *writes) SetLinkPreview(_ context.Context, _, _, id gocql.UUID, preview []byte, off bool) error {
	w.repo.mu.Lock()
	defer w.repo.mu.Unlock()
	w.repo.messages[id].LinkPreview, w.repo.messages[id].NoLinkPreview = preview, off
	return nil
}

func (w *writes) SetLastMessage(context.Context, gocql.UUID, gocql.UUID) error          { return nil }
func (w *writes) SetLastRead(context.Context, gocql.UUID, gocql.UUID, gocql.UUID) error { return nil }
func (w *writes) SavedAmong(context.Context, gocql.UUID, []gocql.UUID) (map[gocql.UUID]bool, error) {
	return nil, nil
}

// previewer records the links it is asked for.
type previewer struct {
	mu   sync.Mutex
	jobs []Unfurl
}

func (p *previewer) Preview(u Unfurl) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobs = append(p.jobs, u)
}

func (p *previewer) take() []Unfurl {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.jobs
	p.jobs = nil
	return out
}

func previewChannel(t *testing.T) (*channel, *messageRepo, *previewer, gocql.UUID) {
	t.Helper()
	a, repo, log, member := newLoggedChannel(t, models.ChannelPublic)
	az, err := authz.New(zap.NewNop(), stringadapter.NewAdapter(
		"p, channel_member, message, read\np, channel_member, message, create\np, channel_member, message, edit_own"))
	if err != nil {
		t.Fatal(err)
	}
	p := &previewer{}
	a.Authz, a.Repo, a.Previews = az, &writes{withLog: log, repo: repo}, p
	a.Cfg.MaxMessageLength = 1000
	return a, repo, p, member
}

func card(url string) *pb.LinkPreview {
	return &pb.LinkPreview{Url: url, Title: "A page", SiteName: "example.com", ImageId: "0123456789abcdef0123456789abcdef"}
}

func TestPostsAskForPreviews(t *testing.T) {
	a, _, p, member := previewChannel(t)
	me := &models.Principal{UserID: member, Role: models.SystemRoleUser}
	m, err := a.post(Post{Target: Target{P: me, ChannelID: a.id}, Content: "look `https://code.example` at https://example.com/a."})
	if err != nil {
		t.Fatal(err)
	}
	jobs := p.take()
	if len(jobs) != 1 || jobs[0].URL != "https://example.com/a" || jobs[0].MessageID.String() != m.Id || jobs[0].ChannelID != a.id {
		t.Fatalf("asked for %+v", jobs)
	}
	if m.LinkPreview != nil {
		t.Fatal("the post waits for nothing")
	}
	if _, err := a.post(Post{Target: Target{P: me, ChannelID: a.id}, Content: "https://example.com/b", NoLinkPreview: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.post(Post{Target: Target{P: me, ChannelID: a.id}, Content: "no link"}); err != nil {
		t.Fatal(err)
	}
	if jobs := p.take(); len(jobs) != 0 {
		t.Fatalf("asked for %+v", jobs)
	}
}

func TestPreviewsAreNumberedAndReplayed(t *testing.T) {
	a, repo, _, member := previewChannel(t)
	m := say(a, repo, member, "see https://example.com/a")
	a.previewed(Previewed{Unfurl: Unfurl{ChannelID: a.id, MessageID: m.ID, URL: "https://example.com/a"}, Preview: card("https://example.com/a")})
	if a.pts != 2 {
		t.Fatalf("pts %d, want 2", a.pts)
	}
	// The same preview again changes nothing.
	a.previewed(Previewed{Unfurl: Unfurl{ChannelID: a.id, MessageID: m.ID, URL: "https://example.com/a"}, Preview: card("https://example.com/a")})
	if a.pts != 2 {
		t.Fatalf("a repeated preview was numbered: pts %d", a.pts)
	}
	d := differenceOf(t, a, member, 1)
	if len(d.Updates) != 1 || d.Pts != 2 {
		t.Fatalf("difference %+v", d)
	}
	got := d.Updates[0].GetMessageEdited().GetMessage()
	if got.GetLinkPreview().GetTitle() != "A page" || got.EditedAt != nil {
		t.Fatalf("replayed %v", got)
	}
}

func TestStalePreviewsAreDropped(t *testing.T) {
	a, repo, _, member := previewChannel(t)
	m := say(a, repo, member, "see https://example.com/new")
	a.previewed(Previewed{Unfurl: Unfurl{ChannelID: a.id, MessageID: m.ID, URL: "https://example.com/old"}, Preview: card("https://example.com/old")})
	off := say(a, repo, member, "https://example.com/a")
	repo.messages[off.ID].NoLinkPreview = true
	a.previewed(Previewed{Unfurl: Unfurl{ChannelID: a.id, MessageID: off.ID, URL: "https://example.com/a"}, Preview: card("https://example.com/a")})
	a.previewed(Previewed{Unfurl: Unfurl{ChannelID: a.id, MessageID: ids.NewTime(), URL: "https://example.com/a"}, Preview: card("https://example.com/a")})
	if a.pts != 2 || repo.messages[m.ID].LinkPreview != nil || repo.messages[off.ID].LinkPreview != nil {
		t.Fatalf("a stale preview was taken: pts %d", a.pts)
	}
}

func TestRemovingAPreviewSticks(t *testing.T) {
	a, repo, p, member := previewChannel(t)
	other := ids.New()
	a.members[other] = models.ChannelRoleMember
	a.snapshot()
	me := &models.Principal{UserID: member, Role: models.SystemRoleUser}
	m := say(a, repo, member, "see https://example.com/a")
	a.previewed(Previewed{Unfurl: Unfurl{ChannelID: a.id, MessageID: m.ID, URL: "https://example.com/a"}, Preview: card("https://example.com/a")})

	_, err := a.removePreview(RemovePreview{Target: Target{P: &models.Principal{UserID: other, Role: models.SystemRoleUser}, ChannelID: a.id}, MessageID: m.ID.String()})
	if !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("someone else removed it: %v", err)
	}
	out, err := a.removePreview(RemovePreview{Target: Target{P: me, ChannelID: a.id}, MessageID: m.ID.String()})
	if err != nil || out.LinkPreview != nil || !repo.messages[m.ID].NoLinkPreview {
		t.Fatalf("removed: %v %v", out, err)
	}
	if a.pts != 3 {
		t.Fatalf("pts %d, want 3", a.pts)
	}
	// Edits bring none back, a new link included.
	if _, err := a.edit(Edit{Target: Target{P: me, ChannelID: a.id}, MessageID: m.ID.String(), Content: "now https://example.com/b"}); err != nil {
		t.Fatal(err)
	}
	if jobs := p.take(); len(jobs) != 0 {
		t.Fatalf("asked for %+v", jobs)
	}
}

func TestEditsFollowTheFirstLink(t *testing.T) {
	a, repo, p, member := previewChannel(t)
	me := &models.Principal{UserID: member, Role: models.SystemRoleUser}
	m := say(a, repo, member, "see https://example.com/a")
	a.previewed(Previewed{Unfurl: Unfurl{ChannelID: a.id, MessageID: m.ID, URL: "https://example.com/a"}, Preview: card("https://example.com/a")})
	edit := func(content string, off bool) *pb.Message {
		t.Helper()
		out, err := a.edit(Edit{Target: Target{P: me, ChannelID: a.id}, MessageID: m.ID.String(), Content: content, NoLinkPreview: off})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// The same link keeps its preview.
	if out := edit("see https://example.com/a, again", false); out.LinkPreview == nil || len(p.take()) != 0 {
		t.Fatal("a kept link lost its preview")
	}
	// Another drops it and asks for its own.
	out := edit("see https://example.com/b", false)
	jobs := p.take()
	if out.LinkPreview != nil || len(jobs) != 1 || jobs[0].URL != "https://example.com/b" {
		t.Fatalf("a new link: %v, asked %+v", out.LinkPreview, jobs)
	}
	// A bot's edit may turn previews off.
	a.previewed(Previewed{Unfurl: jobs[0], Preview: card("https://example.com/b")})
	if out := edit("see https://example.com/c", true); out.LinkPreview != nil || !repo.messages[m.ID].NoLinkPreview || len(p.take()) != 0 {
		t.Fatal("no_link_preview on an edit")
	}
}

func TestBotsAreNotToldOfPreviews(t *testing.T) {
	a, repo, seen, person, _ := botChannel(t, models.ChannelPublic, true)
	m := say(a, repo, person, "see https://example.com/a")
	seen.take()
	a.writesFor(t, repo)
	a.previewed(Previewed{Unfurl: Unfurl{ChannelID: a.id, MessageID: m.ID, URL: "https://example.com/a"}, Preview: card("https://example.com/a")})
	if got := seen.take(); len(got) != 0 {
		t.Fatalf("bots were told: %+v", got)
	}
}

// writesFor lets a test channel store previews.
func (a *channel) writesFor(t *testing.T, repo *messageRepo) {
	t.Helper()
	log, ok := a.Repo.(*withLog)
	if !ok {
		t.Fatalf("repo %T", a.Repo)
	}
	a.Repo = &writes{withLog: log, repo: repo}
}
