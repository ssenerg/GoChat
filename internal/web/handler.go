package web

import (
	"bytes"
	"context"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/cache"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/web/gateway"
	"github.com/ssenerg/gochat/internal/web/render"
	"github.com/ssenerg/gochat/internal/web/views"
)

const meCacheTTL = 30 * time.Second

type Handler struct {
	logger  *zap.Logger
	cfg     *Config
	client  *drpc.Client
	render  *render.Renderer
	engine  *actor.Engine
	gw      *gateway.Deps
	secure  bool
	origin  string
	wsOrig  string
	trusted []*net.IPNet
	auth    *ipLimiter
	reqs    *ipLimiter
	// apiSessions budgets an app's signed-in calls by session, and
	// apiShared by address, at apiSharedFactor times reqs' budget.
	apiSessions *ipLimiter
	apiShared   *ipLimiter
	me          *cache.Cache[string, *pb.User]
	// photoReads and photoWork bound the photo uploads handled at once.
	photoReads chan struct{}
	photoWork  chan struct{}
	connSeq    atomic.Uint64
	csp        string
	zipper     *zipper
	// zipped holds the static files gzipped, by name.
	zipped map[string][]byte
	// botPolls holds the bots' getUpdates that wait.
	botPolls *botPolls
}

func NewHandler(logger *zap.Logger, cfg *Config, client *drpc.Client, r *render.Renderer, engine *actor.Engine, gw *gateway.Deps) *Handler {
	u, _ := url.Parse(cfg.PublicURL)
	h := &Handler{
		logger:   logger.Named("web"),
		cfg:      cfg,
		client:   client,
		render:   r,
		engine:   engine,
		gw:       gw,
		secure:   u.Scheme == "https",
		botPolls: &botPolls{polls: make(map[string]*poll)},
		origin:   u.Scheme + "://" + u.Host,
		auth:     newIPLimiter(rate.Limit(cfg.RateLimit.AuthPerMinute/60), cfg.RateLimit.AuthBurst),
		reqs:     newIPLimiter(rate.Limit(cfg.RateLimit.RequestsPerSecond), cfg.RateLimit.RequestBurst),

		apiSessions: newIPLimiter(rate.Limit(cfg.RateLimit.RequestsPerSecond), cfg.RateLimit.RequestBurst),
		apiShared:   newIPLimiter(rate.Limit(cfg.RateLimit.RequestsPerSecond*apiSharedFactor), cfg.RateLimit.RequestBurst*apiSharedFactor),
		me:          cache.New[string, *pb.User](100_000, meCacheTTL),

		photoReads: make(chan struct{}, photoReaders),
		photoWork:  make(chan struct{}, photoDecoders),
		zipper:     newZipper(2 * runtime.GOMAXPROCS(0)),
	}
	static, _ := fs.Sub(views.Files, "static")
	h.zipped, _ = gzipAll(static)
	wsScheme := "ws"
	if h.secure {
		wsScheme = "wss"
	}
	h.wsOrig = wsScheme + "://" + u.Host
	for _, cidr := range cfg.TrustedProxies {
		_, n, _ := net.ParseCIDR(cidr)
		h.trusted = append(h.trusted, n)
	}
	h.csp = strings.Join([]string{
		"default-src 'none'",
		// WebAssembly only, for the voice client's noise filter.
		"script-src 'self' 'wasm-unsafe-eval'",
		"style-src 'self'",
		"img-src 'self' data:",
		"media-src 'self'",
		"connect-src " + strings.Join(sources(append([]string{"'self'", h.wsOrig}, cfg.VoiceOrigins...)), " "),
		"form-action 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"manifest-src 'self'",
	}, "; ")
	return h
}

// sources drops the repeats from a CSP source list, such as a voice server
// behind the same host as the gateway.
func sources(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := list[:0]
	for _, src := range list {
		key := strings.TrimSuffix(strings.ToLower(src), "/")
		if !seen[key] {
			seen[key] = true
			out = append(out, src)
		}
	}
	return out
}

// Routes returns the gateway's whole HTTP surface.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(views.Files, "static")
	mux.Handle("GET "+h.render.StaticPrefix, h.staticFiles(http.StripPrefix(h.render.StaticPrefix, http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	mux.HandleFunc("GET /login", h.loginPage)
	mux.Handle("POST /login", h.limited(h.csrf(http.HandlerFunc(h.login))))
	mux.HandleFunc("GET /login/mfa", h.mfaPage)
	mux.Handle("POST /login/mfa", h.limited(h.csrf(http.HandlerFunc(h.mfa))))
	mux.HandleFunc("GET /signup", h.signupPage)
	mux.Handle("POST /signup", h.limited(h.csrf(http.HandlerFunc(h.signup))))
	mux.Handle("POST /logout", h.csrf(http.HandlerFunc(h.logout)))
	mux.Handle("GET /join/{token}", h.limited(http.HandlerFunc(h.joinPage)))
	mux.Handle("POST /join/{token}", h.limited(h.csrf(http.HandlerFunc(h.join))))

	mux.HandleFunc("GET /{$}", h.home)
	mux.HandleFunc("GET /teams/new", h.newTeamPage)
	mux.Handle("POST /teams", h.csrf(http.HandlerFunc(h.createTeam)))
	mux.HandleFunc("GET /t/{team}", h.teamHome)
	mux.HandleFunc("GET /t/{team}/ch/{channel}", h.channelPage)
	mux.HandleFunc("GET /t/{team}/ch/{channel}/messages", h.olderMessages)
	mux.HandleFunc("GET /t/{team}/ch/{channel}/members", h.channelMembers)
	mux.HandleFunc("GET /t/{team}/ch/{channel}/add", h.addMemberModal)
	mux.HandleFunc("GET /t/{team}/ch/{channel}/add/search", h.addMemberSearch)
	mux.Handle("POST /t/{team}/ch/{channel}/members", h.csrf(http.HandlerFunc(h.addMember)))
	mux.Handle("POST /t/{team}/ch/{channel}/members/{user}/remove", h.csrf(http.HandlerFunc(h.removeMember)))
	mux.Handle("POST /t/{team}/ch/{channel}/join", h.csrf(http.HandlerFunc(h.joinChannel)))
	mux.Handle("POST /t/{team}/ch/{channel}/leave", h.csrf(http.HandlerFunc(h.leaveChannel)))
	mux.HandleFunc("GET /t/{team}/ch/{channel}/settings", h.channelSettings)
	mux.Handle("POST /t/{team}/ch/{channel}/settings", h.csrf(http.HandlerFunc(h.updateChannel)))
	mux.Handle("POST /t/{team}/ch/{channel}/privacy", h.csrf(http.HandlerFunc(h.channelPrivacy)))
	mux.Handle("POST /t/{team}/ch/{channel}/archive", h.csrf(http.HandlerFunc(h.archiveChannel)))
	mux.Handle("POST /t/{team}/ch/{channel}/voice", h.csrf(http.HandlerFunc(h.setChannelVoice)))
	mux.HandleFunc("GET /t/{team}/ch/{channel}/delete", h.deleteConversationModal)
	mux.Handle("POST /t/{team}/ch/{channel}/delete", h.csrf(http.HandlerFunc(h.deleteConversation)))
	mux.HandleFunc("GET /t/{team}/channels/new", h.newChannelModal)
	mux.Handle("POST /t/{team}/channels", h.csrf(http.HandlerFunc(h.createChannel)))
	mux.HandleFunc("GET /t/{team}/channels/browse", h.browseModal)
	mux.HandleFunc("GET /t/{team}/switch", h.switcher)
	mux.HandleFunc("GET /t/{team}/switch/results", h.switcherResults)
	mux.HandleFunc("GET /t/{team}/search", h.searchPanel)
	mux.HandleFunc("GET /t/{team}/search/more", h.searchMore)
	mux.HandleFunc("GET /t/{team}/people", h.peopleModal)
	mux.HandleFunc("GET /t/{team}/people/search", h.peopleSearch)
	mux.Handle("POST /t/{team}/dm", h.csrf(http.HandlerFunc(h.openDirect)))
	mux.HandleFunc("GET /t/{team}/invite", h.inviteModal)
	mux.Handle("POST /t/{team}/invites", h.csrf(http.HandlerFunc(h.createInvite)))
	mux.HandleFunc("GET /t/{team}/invites", h.moreInvites)
	mux.Handle("POST /t/{team}/invites/{invite}/revoke", h.csrf(http.HandlerFunc(h.revokeInvite)))
	mux.HandleFunc("GET /t/{team}/members", h.teamMembers)
	mux.HandleFunc("GET /t/{team}/settings", h.teamSettings)
	mux.Handle("POST /t/{team}/photo", h.csrfMultipart(http.HandlerFunc(h.setTeamPhoto)))
	mux.Handle("POST /t/{team}/photo/remove", h.csrf(http.HandlerFunc(h.setTeamPhoto)))
	mux.Handle("POST /t/{team}/members/{user}/remove", h.csrf(http.HandlerFunc(h.removeTeamMember)))
	mux.Handle("POST /t/{team}/members/{user}/role", h.csrf(http.HandlerFunc(h.teamMemberRole)))

	mux.HandleFunc("GET /m/{channel}/{message}", h.messageFragment)
	mux.HandleFunc("GET /m/{channel}/{message}/thread", h.threadPanel)
	mux.Handle("POST /m/{channel}/{message}/follow", h.csrf(http.HandlerFunc(h.followThread)))
	mux.Handle("POST /m/{channel}/{message}/pin", h.csrf(http.HandlerFunc(h.pinMessage)))
	mux.Handle("POST /m/{channel}/{message}/save", h.csrf(http.HandlerFunc(h.saveMessage)))
	mux.HandleFunc("GET /t/{team}/ch/{channel}/pins", h.pinsPanel)
	mux.HandleFunc("GET /t/{team}/saved", h.savedPanel)
	mux.HandleFunc("GET /t/{team}/threads", h.threadsPage)
	mux.HandleFunc("GET /m/{channel}/people", h.mentionSuggestions)
	mux.HandleFunc("GET /m/{channel}/commands", h.commandSuggestions)
	mux.HandleFunc("GET /c/{channel}", h.channelLink)
	mux.Handle("POST /t/{team}/ch/{channel}/mute", h.csrf(http.HandlerFunc(h.muteChannel)))
	mux.HandleFunc("GET /m/{channel}/{message}/react", h.reactionPicker)
	mux.Handle("POST /m/{channel}/{message}/react", h.csrf(http.HandlerFunc(h.react)))
	mux.HandleFunc("GET /m/{channel}/{message}/edit", h.editMessageForm)
	mux.Handle("POST /m/{channel}/{message}", h.csrf(http.HandlerFunc(h.editMessage)))
	mux.Handle("POST /m/{channel}/{message}/delete", h.csrf(http.HandlerFunc(h.deleteMessage)))

	mux.Handle("POST /voice/join", h.csrf(http.HandlerFunc(h.joinVoice)))
	mux.Handle("POST /voice/leave", h.csrf(http.HandlerFunc(h.leaveVoice)))
	mux.Handle("POST /voice/state", h.csrf(http.HandlerFunc(h.voiceState)))
	mux.Handle("POST /t/{team}/ch/{channel}/voice/{user}/disconnect", h.csrf(http.HandlerFunc(h.disconnectVoice)))

	mux.Handle("POST /upload", h.csrfMultipart(http.HandlerFunc(h.upload)))
	mux.HandleFunc("GET /files/{id}", h.download)
	mux.HandleFunc("GET /files/{id}/inline", h.download)
	mux.HandleFunc("GET /photos/{kind}/{id}/{version}", h.servePhoto)

	mux.HandleFunc("GET /settings", h.settingsPage)
	mux.Handle("POST /settings/profile", h.csrf(http.HandlerFunc(h.updateProfile)))
	mux.HandleFunc("GET /status", h.statusModal)
	mux.Handle("POST /status", h.csrf(http.HandlerFunc(h.setStatus)))
	mux.Handle("POST /settings/photo", h.csrfMultipart(http.HandlerFunc(h.setPhoto)))
	mux.Handle("POST /settings/photo/remove", h.csrf(http.HandlerFunc(h.setPhoto)))
	mux.Handle("POST /settings/password", h.csrf(http.HandlerFunc(h.changePassword)))
	mux.Handle("POST /settings/totp/begin", h.csrf(http.HandlerFunc(h.beginTOTP)))
	mux.Handle("POST /settings/totp/confirm", h.csrf(http.HandlerFunc(h.confirmTOTP)))
	mux.Handle("POST /settings/totp/disable", h.csrf(http.HandlerFunc(h.disableTOTP)))
	mux.Handle("POST /settings/sessions/{id}/revoke", h.csrf(http.HandlerFunc(h.revokeSession)))
	mux.Handle("POST /settings/sessions/revoke", h.csrf(http.HandlerFunc(h.revokeOtherSessions)))
	mux.Handle("POST /settings/bots", h.csrf(http.HandlerFunc(h.createBot)))
	mux.Handle("POST /settings/bots/{bot}", h.csrf(http.HandlerFunc(h.updateBot)))
	mux.Handle("POST /settings/bots/{bot}/token", h.csrf(http.HandlerFunc(h.resetBotToken)))
	mux.Handle("POST /settings/bots/{bot}/delete", h.csrf(http.HandlerFunc(h.deleteBot)))
	mux.Handle("POST /settings/bots/{bot}/team", h.csrf(http.HandlerFunc(h.addBotToTeam)))

	mux.HandleFunc("GET /admin/users", h.adminUsers)
	mux.Handle("POST /admin/users/{user}/deactivate", h.csrf(http.HandlerFunc(h.adminDeactivate)))
	mux.Handle("POST /admin/users/{user}/sessions/revoke", h.csrf(http.HandlerFunc(h.adminRevokeSessions)))
	mux.Handle("POST /admin/users/{user}/quota", h.csrf(http.HandlerFunc(h.adminQuota)))

	mux.HandleFunc("GET /ws", h.websocket)

	// The client API, for native apps; see client.proto.
	mux.HandleFunc("POST /api/v1/rpc/{service}/{method}", h.apiRPC)
	mux.HandleFunc("GET /api/v1/gateway", h.apiGateway)
	mux.HandleFunc("GET /api/v1/info", h.apiInfo)
	mux.HandleFunc("POST /api/v1/files", h.apiUpload)
	mux.HandleFunc("GET /api/v1/files/{id}", h.apiDownload)
	mux.HandleFunc("GET /api/v1/photos/{kind}/{id}/{version}", h.apiPhoto)
	mux.HandleFunc("POST /api/v1/photo", h.apiSetPhoto)
	mux.HandleFunc("GET /api/v2/socket", h.apiSocket)
	mux.HandleFunc("POST /api/v2/files", h.apiUpload)
	mux.HandleFunc("GET /api/v2/files/{id}", h.apiDownload)
	mux.HandleFunc("POST /api/v2/bot/{method...}", h.botAPI)

	return h.recover(h.headers(h.throttle(mux)))
}

func (h *Handler) staticFiles(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Files only, never a listing of them.
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		hd := w.Header()
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
		hd.Add("Vary", "Accept-Encoding")
		name := strings.TrimPrefix(r.URL.Path, h.render.StaticPrefix)
		if zipped, ok := h.zipped[name]; ok && acceptsGzip(r) {
			hd.Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
			hd.Set("Content-Encoding", "gzip")
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(zipped))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the peer's address, or with a trusted proxy in front, the
// right-most address in X-Forwarded-For that is not one of the proxies.
func (h *Handler) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if len(h.trusted) == 0 || !h.isTrusted(net.ParseIP(host)) {
		return host
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break
		}
		if !h.isTrusted(ip) {
			return ip.String()
		}
	}
	return host
}

func (h *Handler) isTrusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range h.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (h *Handler) call(r *http.Request) drpc.Call {
	ua := r.UserAgent()
	if len(ua) > 256 {
		ua = ua[:256]
	}
	return drpc.Call{Session: h.sessionToken(r), ClientIP: h.clientIP(r), UserAgent: ua}
}

func (h *Handler) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return h.client.Context(r.Context(), h.call(r))
}

// currentUser returns the signed-in user, or nil.
// currentUser returns the signed-in user, or nil. Only a failure to find
// out, such as the server restarting, is an error.
func (h *Handler) currentUser(r *http.Request) (*pb.User, error) {
	token := h.sessionToken(r)
	if token == "" {
		return nil, nil
	}
	if u, ok := h.me.Get(token); ok {
		return u, nil
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Auth.Me(ctx, &pb.MeRequest{})
	if err != nil {
		if drpc.Err(err).Code == ierrors.CodeUnauthenticated {
			return nil, nil
		}
		return nil, err
	}
	h.me.Set(token, res.User)
	return res.User, nil
}

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// safeNext keeps a post-login redirect on this site.
func safeNext(next string) string {
	if next == "" || len(next) > 2048 || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	// Browsers drop tabs and newlines from URLs and read a backslash as a
	// slash, either of which can turn a path into another host.
	for i := range len(next) {
		if c := next[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return "/"
	}
	return next
}
