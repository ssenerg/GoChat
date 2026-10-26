package bots

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"syscall"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/updates"
)

// SecretHeader carries a webhook's secret on each delivery.
const SecretHeader = "X-GoChat-Bot-Secret" //nolint:gosec // a header name, not a secret

const (
	maxWebhookURL = 2048
	// deliveryBatch is how many updates one read of the log brings.
	deliveryBatch = 100
	// idleCheck is how often a webhook with nothing to deliver looks again,
	// should a nudge be lost.
	idleCheck = 30 * time.Second
)

var secretRe = regexp.MustCompile(`^[A-Za-z0-9_-]{0,256}$`)

// JSON is how the HTTP Bot API and webhooks write updates: protobuf's JSON
// mapping, with the proto field names.
var JSON = protojson.MarshalOptions{UseProtoNames: true}

func (w *worker) setWebhook(ctx context.Context, msg SetWebhook) (Done, error) {
	if !msg.P.Bot {
		return Done{}, ierrors.PermissionDenied()
	}
	if msg.URL != "" {
		if err := checkWebhook(msg.URL, w.Cfg.AllowPrivateWebhooks); err != nil {
			return Done{}, err
		}
		if !secretRe.MatchString(msg.Secret) {
			return Done{}, ierrors.InvalidArgument("the secret is up to 256 letters, digits, '_' and '-'")
		}
		// A new webhook gets the updates after it, not those the bot read
		// over its socket.
		b, err := w.Repo.GetBot(ctx, msg.P.UserID)
		if err != nil {
			return Done{}, err
		}
		if b.WebhookURL == "" {
			pts, err := w.Repo.AccountPts(ctx, msg.P.UserID)
			if err != nil {
				return Done{}, err
			}
			if err := w.Repo.SetDelivered(ctx, msg.P.UserID, pts); err != nil {
				return Done{}, err
			}
		}
	}
	if err := w.Repo.SetWebhook(ctx, msg.P.UserID, msg.URL, msg.Secret); err != nil {
		return Done{}, err
	}
	if w.Webhooks != nil {
		w.Engine.Send(w.Webhooks, Hooked{BotID: msg.P.UserID})
	}
	return Done{}, nil
}

func (w *worker) webhookInfo(ctx context.Context, p *models.Principal) (*pb.GetWebhookInfoResponse, error) {
	if !p.Bot {
		return nil, ierrors.PermissionDenied()
	}
	b, err := w.Repo.GetBot(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	pts, err := w.Repo.AccountPts(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return &pb.GetWebhookInfoResponse{
		Url: b.WebhookURL, DeliveredPts: b.DeliveredPts, Pending: max(pts-b.DeliveredPts, 0),
		LastError: b.WebhookError, LastErrorAt: timeOf(b.WebhookErrorAt),
	}, nil
}

func (w *worker) confirm(ctx context.Context, msg Confirm) (Done, error) {
	if !msg.P.Bot {
		return Done{}, ierrors.PermissionDenied()
	}
	if msg.Pts < 0 {
		return Done{}, ierrors.InvalidArgument("invalid pts")
	}
	latest, err := w.Repo.AccountPts(ctx, msg.P.UserID)
	if err != nil {
		return Done{}, err
	}
	return Done{}, w.Repo.SetDelivered(ctx, msg.P.UserID, min(msg.Pts, latest))
}

func timeOf(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// checkWebhook refuses a webhook URL that is not https, carries
// credentials, or names an address that is not public.
func checkWebhook(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || len(raw) > maxWebhookURL || u.Host == "" || u.User != nil || u.Fragment != "":
		return ierrors.InvalidArgument("the webhook is an https URL, without credentials")
	case u.Scheme != "https" && (!allowPrivate || u.Scheme != "http"):
		return ierrors.InvalidArgument("the webhook is an https URL")
	}
	if addr, err := netip.ParseAddr(u.Hostname()); err == nil && !allowPrivate && !Public(addr) {
		return ierrors.InvalidArgument("the webhook must reach a public address")
	}
	return nil
}

// blocked are the ranges no webhook may reach besides those Public tests
// by kind: shared, benchmark, reserved and translated addresses.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// Public reports whether addr is a public unicast address a webhook may
// reach: not loopback, private, link-local, multicast or reserved, nor one
// of those wrapped in IPv6.
func Public(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	// 6to4 carries an IPv4 address that must be public too.
	if addr.Is6() && addr.As16()[0] == 0x20 && addr.As16()[1] == 0x02 {
		b := addr.As16()
		return Public(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	return true
}

// errNotPublic refuses a connection to an address webhooks may not reach.
var errNotPublic = errors.New("the webhook resolves to an address that is not public")

// Client is the HTTP client webhooks are delivered with: it checks every
// address it connects to, after resolving, so a name cannot point it at
// the server's neighbours; it takes no proxy and follows no redirect.
func Client(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, Control: func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !Public(ap.Addr()) {
			return errNotPublic
		}
		return nil
	}}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true,
			TLSHandshakeTimeout: timeout, MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Minute,
			ResponseHeaderTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type (
	// hookDone stops a bot's delivery after it ended.
	hookDone struct {
		botID gocql.UUID
		pid   *actor.PID
	}
	start struct{}
)

// WebhookDeps is what the webhooks share.
type WebhookDeps struct {
	*Deps
	Updates *actor.PID
	Client  *http.Client
}

// Webhooks runs one delivery per bot with a webhook.
func Webhooks(d *WebhookDeps) actor.Producer {
	return func() actor.Receiver {
		return &webhooks{WebhookDeps: d, hooks: make(map[gocql.UUID]*actor.PID)}
	}
}

type webhooks struct {
	*WebhookDeps
	hooks map[gocql.UUID]*actor.PID
}

func (h *webhooks) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		c.Send(c.PID(), start{})
	case start:
		go h.startAll(c.Engine(), c.PID())
	case Hooked:
		if pid := h.hooks[msg.BotID]; pid != nil {
			c.Send(pid, Hooked{BotID: msg.BotID})
			return
		}
		pid := c.SpawnChild(newHook(h.WebhookDeps, msg.BotID, c.PID()), "hook", actor.WithID(msg.BotID.String()),
			actor.WithMiddleware(actors.Recover(h.Logger)))
		h.hooks[msg.BotID] = pid
		c.Send(h.Updates, updates.Listen{UserID: msg.BotID, PID: pid})
	case hookDone:
		if pid := h.hooks[msg.botID]; pid != nil && pid.Equals(msg.pid) {
			delete(h.hooks, msg.botID)
			c.Send(h.Updates, updates.Unlisten{UserID: msg.botID})
			c.Engine().Poison(pid)
		}
	}
}

// startAll starts the delivery of every bot with a webhook.
func (h *webhooks) startAll(engine *actor.Engine, self *actor.PID) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	list, err := h.Repo.ListWebhooks(ctx)
	if err != nil {
		h.Logger.Error("failed to list the webhooks", zap.Error(err))
		return
	}
	for _, b := range list {
		engine.Send(self, Hooked{BotID: b.ID})
	}
}

// hook delivers one bot's updates to its webhook, in order: each waits
// until the one before was taken.
type hook struct {
	*WebhookDeps
	botID  gocql.UUID
	parent *actor.PID
	wake   chan struct{}
	cancel context.CancelFunc
}

func newHook(d *WebhookDeps, botID gocql.UUID, parent *actor.PID) actor.Producer {
	return func() actor.Receiver {
		return &hook{WebhookDeps: d, botID: botID, parent: parent, wake: make(chan struct{}, 1)}
	}
}

func (k *hook) Receive(c *actor.Context) {
	switch c.Message().(type) {
	case actor.Started:
		ctx, cancel := context.WithCancel(context.Background())
		k.cancel = cancel
		self, engine := c.PID(), c.Engine()
		go func() {
			k.run(ctx)
			engine.Send(k.parent, hookDone{botID: k.botID, pid: self})
		}()
	case actor.Stopped:
		k.cancel()
	case updates.Nudge, Hooked:
		select {
		case k.wake <- struct{}{}:
		default:
		}
	}
}

// run delivers until the bot has no webhook.
func (k *hook) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		b, err := k.bot(ctx)
		if err != nil {
			k.Logger.Warn("failed to read a bot's webhook", zap.String("bot", k.botID.String()), zap.Error(err))
			k.sleep(ctx, backoff)
			continue
		}
		if b == nil || b.WebhookURL == "" {
			return
		}
		d, err := k.difference(ctx, b.DeliveredPts)
		if err != nil {
			k.Logger.Warn("failed to read a bot's updates", zap.String("bot", k.botID.String()), zap.Error(err))
			k.sleep(ctx, backoff)
			continue
		}
		if d.TooLong {
			k.Logger.Warn("a webhook fell behind what is kept", zap.String("bot", k.botID.String()))
			k.confirm(ctx, d.Pts)
			continue
		}
		if len(d.Updates) == 0 {
			// Updates about messages deleted since are passed over.
			if d.Pts > b.DeliveredPts {
				k.confirm(ctx, d.Pts)
				continue
			}
			k.idle(ctx)
			continue
		}
		for _, ev := range d.Updates {
			if err := k.deliver(ctx, b, ev); err != nil {
				k.failed(ctx, b, err)
				k.sleep(ctx, backoff)
				backoff = min(backoff*2, k.Cfg.WebhookRetryMax)
				break
			}
			backoff = time.Second
			if b.WebhookError != "" {
				b.WebhookError = ""
				k.failed(ctx, b, nil)
			}
			k.confirm(ctx, ev.Pts)
		}
	}
}

func (k *hook) bot(ctx context.Context) (*models.Bot, error) {
	ctx, cancel := context.WithTimeout(ctx, k.Cfg.OpTimeout)
	defer cancel()
	b, err := k.Repo.GetBot(ctx, k.botID)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return nil, nil
	}
	return b, err
}

func (k *hook) difference(ctx context.Context, pts int64) (*updates.Difference, error) {
	ctx, cancel := context.WithTimeout(ctx, k.Cfg.OpTimeout)
	defer cancel()
	return actors.Ask[*updates.Difference](ctx, k.Engine, k.Updates, updates.GetDifference{UserID: k.botID, Pts: pts, Limit: deliveryBatch})
}

func (k *hook) confirm(ctx context.Context, pts int64) {
	ctx, cancel := context.WithTimeout(ctx, k.Cfg.OpTimeout)
	defer cancel()
	if err := k.Repo.SetDelivered(ctx, k.botID, pts); err != nil {
		k.Logger.Warn("failed to record a delivery", zap.String("bot", k.botID.String()), zap.Error(err))
	}
}

func (k *hook) failed(ctx context.Context, b *models.Bot, cause error) {
	reason := ""
	if cause != nil {
		reason = cause.Error()
		b.WebhookError = reason
	}
	ctx, cancel := context.WithTimeout(ctx, k.Cfg.OpTimeout)
	defer cancel()
	if err := k.Repo.SetWebhookError(ctx, k.botID, reason, time.Now().UTC()); err != nil {
		k.Logger.Warn("failed to record a webhook's failure", zap.String("bot", k.botID.String()), zap.Error(err))
	}
}

// deliver POSTs one update; a 2xx answer takes it.
func (k *hook) deliver(ctx context.Context, b *models.Bot, ev *pb.Event) error {
	body, err := JSON.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GoChat-Webhook")
	if b.WebhookSecret != "" {
		req.Header.Set(SecretHeader, b.WebhookSecret)
	}
	res, err := k.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			// The error names the URL, which may hold a secret path.
			return fmt.Errorf("delivery failed: %w", ue.Err)
		}
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("the webhook answered %d", res.StatusCode)
	}
	return nil
}

func (k *hook) idle(ctx context.Context) {
	t := time.NewTimer(idleCheck)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-k.wake:
	case <-t.C:
	}
}

func (k *hook) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
