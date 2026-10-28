// Package previews makes the cards under messages with links: it fetches
// the first link of a message off the post's path, reads what the page
// says of itself, keeps its picture, and hands the preview to the
// message's channel, which stores it and tells the members.
package previews

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"io"
	"net/url"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/photo"
	"github.com/ssenerg/gochat/internal/core/unfurl"
	"github.com/ssenerg/gochat/internal/services/channels"
)

const (
	// imageSide bounds a stored picture, minImageSide refuses icons and
	// tracking pixels.
	imageSide    = 640
	minImageSide = 32
	// decoders bounds the pictures decoded at once: one can take ~100MB.
	decoders = 2
	// MaxImage bounds a stored picture.
	MaxImage = 2 << 20
)

type Repo interface {
	GetLinkPreview(ctx context.Context, urlHash []byte) ([]byte, bool, error)
	PutLinkPreview(ctx context.Context, urlHash, preview []byte, ttl time.Duration) error
}

type Storage interface {
	Put(ctx context.Context, key string, r io.Reader, contentType string) (int64, error)
}

type Deps struct {
	Logger  *zap.Logger
	Cfg     *Config
	Repo    Repo
	Storage Storage
	Engine  *actor.Engine
	// Channels takes the previews made, as channels.Previewed.
	Channels *actor.PID
	Fetcher  *unfurl.Fetcher
}

// Service queues links for its workers. It is the channels' Previewer.
type Service struct {
	*Deps
	PID      *actor.PID
	pending  atomic.Int64
	flights  singleflight.Group
	decoding chan struct{}
}

var _ channels.Previewer = (*Service)(nil)

// Fetcher fetches as the configuration says.
func Fetcher(cfg *Config) *unfurl.Fetcher {
	return &unfurl.Fetcher{Timeout: cfg.Timeout, MaxRedirects: cfg.MaxRedirects, Ports: cfg.Ports, UserAgent: cfg.UserAgent}
}

// Start spawns the workers.
func Start(d *Deps) *Service {
	s := &Service{Deps: d, decoding: make(chan struct{}, decoders)}
	s.PID = d.Engine.Spawn(actors.NewPool(d.Cfg.Workers, func() actor.Receiver { return &worker{s: s} },
		actor.WithMiddleware(actors.Recover(d.Logger))), "previews", actor.WithID("main"))
	return s
}

// Preview queues a link, unless the queue is full.
func (s *Service) Preview(job channels.Unfurl) {
	if s.pending.Add(1) > int64(s.Cfg.Queue) {
		s.pending.Add(-1)
		s.Logger.Debug("link preview queue full", zap.String("channel", job.ChannelID.String()))
		return
	}
	s.Engine.Send(s.PID, job)
}

type worker struct{ s *Service }

func (w *worker) Receive(c *actor.Context) {
	if job, ok := c.Message().(channels.Unfurl); ok {
		defer w.s.pending.Add(-1)
		w.s.unfurl(job)
	}
}

func (s *Service) unfurl(job channels.Unfurl) {
	u, err := url.Parse(job.URL)
	if err != nil || !s.Cfg.Hosts().Allows(u.Hostname()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*s.Cfg.Timeout+10*time.Second)
	defer cancel()
	sum := sha256.Sum256([]byte(job.URL))
	key := sum[:]
	data, found, err := s.Repo.GetLinkPreview(ctx, key)
	if err != nil {
		s.Logger.Warn("failed to read a cached link preview", zap.Error(err))
		return
	}
	if !found {
		v, _, _ := s.flights.Do(string(key), func() (any, error) { return s.make(ctx, key, job.URL), nil })
		data, _ = v.([]byte)
	}
	if len(data) == 0 {
		return
	}
	var preview pb.LinkPreview
	if err := proto.Unmarshal(data, &preview); err != nil {
		return
	}
	s.Engine.Send(s.Channels, channels.Previewed{Unfurl: job, Preview: &preview})
}

// make fetches a link's preview and caches it, or that it has none.
func (s *Service) make(ctx context.Context, key []byte, link string) []byte {
	preview, err := s.fetch(ctx, link)
	if err != nil {
		s.Logger.Debug("no link preview", zap.String("url", link), zap.Error(err))
	}
	var data []byte
	ttl := s.Cfg.FailureTTL
	if preview != nil {
		data, _ = proto.Marshal(preview)
		ttl = s.Cfg.CacheTTL
	}
	if err := s.Repo.PutLinkPreview(ctx, key, data, ttl); err != nil {
		s.Logger.Warn("failed to cache a link preview", zap.Error(err))
	}
	return data
}

// fetch reads a page and makes its preview, nil when it says nothing.
func (s *Service) fetch(ctx context.Context, link string) (*pb.LinkPreview, error) {
	hosts := s.Cfg.Hosts()
	res, err := s.Fetcher.Fetch(ctx, unfurl.Request{
		URL: link, Accept: "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1", Takes: unfurl.IsHTML,
		MaxBytes: s.Cfg.MaxPageSize, Truncate: true, Host: hosts.Allows,
	})
	if err != nil {
		return nil, err
	}
	page := unfurl.Parse(res.Body, res.ContentType, res.URL)
	if page.Empty() {
		return nil, nil
	}
	out := &pb.LinkPreview{Url: link, Title: page.Title, Description: page.Description, SiteName: page.SiteName}
	if page.Image != "" {
		id, size, err := s.image(ctx, page.Image, func(h string) bool { return !hosts.Denies(h) })
		if err != nil {
			s.Logger.Debug("no picture for a link preview", zap.String("url", page.Image), zap.Error(err))
		} else {
			out.ImageId, out.ImageWidth, out.ImageHeight = id, uint32(size.X), uint32(size.Y) //nolint:gosec // at most imageSide
		}
	}
	return out, nil
}

var errSmall = errors.New("the picture is too small")

// image fetches a page's picture, turns it into one safe to serve, and
// stores it under its id.
func (s *Service) image(ctx context.Context, src string, host func(string) bool) (string, image.Point, error) {
	res, err := s.Fetcher.Fetch(ctx, unfurl.Request{
		URL: src, Accept: "image/webp,image/png,image/jpeg,image/gif;q=0.9,*/*;q=0.1", Takes: unfurl.IsImage,
		MaxBytes: s.Cfg.MaxImageSize, Host: host,
	})
	if err != nil {
		return "", image.Point{}, err
	}
	select {
	case s.decoding <- struct{}{}:
	case <-ctx.Done():
		return "", image.Point{}, ctx.Err()
	}
	data, ctype, size, err := photo.Fit(res.Body, imageSide, imageSide)
	<-s.decoding
	if err != nil {
		return "", image.Point{}, err
	}
	if size.X < minImageSide || size.Y < minImageSide {
		return "", image.Point{}, errSmall
	}
	if len(data) > MaxImage {
		return "", image.Point{}, photo.ErrTooLarge
	}
	id := ImageID(data)
	if _, err := s.Storage.Put(ctx, imageKey(id), bytes.NewReader(data), ctype); err != nil {
		return "", image.Point{}, err
	}
	return id, size, nil
}

// ImageID names a stored picture by its content.
func ImageID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

var imageIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ImageKey is where a picture is stored, false for an id no picture has.
func ImageKey(id string) (string, bool) {
	if !imageIDRe.MatchString(id) {
		return "", false
	}
	return imageKey(id), true
}

func imageKey(id string) string { return "previews/" + id }
