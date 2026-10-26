// Package accounts authenticates people: sign up, login with an optional
// TOTP second factor, sessions and their revocation. It runs as a pool of
// workers, which also bounds how many password hashes run at once.
package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"net/netip"
	"strings"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/cache"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/keylock"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/password"
	"github.com/ssenerg/gochat/internal/core/secure"
	"github.com/ssenerg/gochat/internal/core/totp"
	"github.com/ssenerg/gochat/internal/core/validate"
	"github.com/ssenerg/gochat/internal/core/window"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/invites"
	"github.com/ssenerg/gochat/internal/services/users"
	"github.com/ssenerg/gochat/internal/services/voice"
)

type Repo interface {
	CreateUser(ctx context.Context, u *models.User) error
	GetUser(ctx context.Context, id gocql.UUID) (*models.User, error)
	GetUserIDByUsername(ctx context.Context, username string) (gocql.UUID, error)
	GetUserIDByEmail(ctx context.Context, email string) (gocql.UUID, error)
	UpdatePassword(ctx context.Context, id gocql.UUID, hash string, now time.Time) error
	SetTOTPPending(ctx context.Context, id gocql.UUID, sealed []byte, now time.Time) error
	EnableTOTP(ctx context.Context, id gocql.UUID, sealed []byte, step int64, now time.Time) (bool, error)
	DisableTOTP(ctx context.Context, id gocql.UUID, now time.Time) error
	AdvanceTOTPStep(ctx context.Context, id gocql.UUID, step int64) (bool, error)
	ReplaceRecoveryCodes(ctx context.Context, id gocql.UUID, hashes [][]byte) error
	ConsumeRecoveryCode(ctx context.Context, id gocql.UUID, hash []byte) (bool, error)
	ClaimBootstrap(ctx context.Context, id gocql.UUID) (bool, error)
	ReleaseBootstrap(ctx context.Context, id gocql.UUID) error
	Bootstrapped(ctx context.Context) (bool, error)

	CreateSession(ctx context.Context, s *models.Session) error
	GetSession(ctx context.Context, hash []byte) (*models.Session, error)
	DeleteSession(ctx context.Context, hash []byte, userID gocql.UUID) error
	DeleteUserSessions(ctx context.Context, userID gocql.UUID, keep []byte) ([][]byte, error)
	ListUserSessions(ctx context.Context, userID gocql.UUID) ([]*models.Session, error)
	TouchSession(ctx context.Context, s *models.Session, at time.Time, ip string) error
	SetDeactivated(ctx context.Context, id gocql.UUID, deactivated bool, now time.Time) error
	CreateMFAChallenge(ctx context.Context, c *models.MFAChallenge, now time.Time) error
	GetMFAChallenge(ctx context.Context, hash []byte) (*models.MFAChallenge, error)
	CountMFAAttempt(ctx context.Context, c *models.MFAChallenge, now time.Time) (bool, error)
	ConsumeMFAChallenge(ctx context.Context, hash []byte) (bool, error)

	GetTeam(ctx context.Context, id gocql.UUID) (*models.Team, error)
	GetTeamMember(ctx context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error)
	GetInvite(ctx context.Context, hash []byte) (*models.Invite, error)
	BotOfToken(ctx context.Context, hash []byte) (gocql.UUID, error)
	ListBotIDs(ctx context.Context, owner gocql.UUID) ([]gocql.UUID, error)
	GetBot(ctx context.Context, id gocql.UUID) (*models.Bot, error)
	ConsumeInvite(ctx context.Context, inv *models.Invite, userID gocql.UUID, now time.Time) (bool, error)
	ReleaseInvite(ctx context.Context, inv *models.Invite, userID gocql.UUID, now time.Time) error
}

const recoveryCodeCount = 10

// accountFailureFactor is how many more failed sign-ins an account takes from
// all addresses together than from one.
const accountFailureFactor = 10

// Deps is what every worker of the pool shares.
type Deps struct {
	Logger *zap.Logger
	Cfg    *Config
	Repo   Repo
	Users  *users.Cache
	Authz  *authz.Authorizer
	Engine *actor.Engine
	Hub    *actor.PID
	// Voice hears of revoked sessions too, which may talk in voice channels.
	Voice  *actor.PID
	sealer *secure.Sealer
	// recoveryKey keys the hashes of recovery codes, which are too short to
	// be stored under a plain hash.
	recoveryKey []byte
	sessions    *cache.Cache[string, *models.Principal]
	// revokedSessions keeps a revoked session's hash for a while, so a lookup
	// that raced the revocation does not cache it again.
	revokedSessions *cache.Cache[string, struct{}]
	failures        *window.Counter
	// accountFailures counts failed sign-ins per account from every address.
	accountFailures *window.Counter
	// totp serializes each user's two-factor changes, so the recovery codes
	// a confirmation shows are the ones it stored.
	totp *keylock.Locks
}

func NewDeps(logger *zap.Logger, cfg *Config, repo Repo, userCache *users.Cache, az *authz.Authorizer, engine *actor.Engine, hubPID *actor.PID) (*Deps, error) {
	key, err := secure.DecodeKey(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	sealer, err := secure.NewSealer(key)
	if err != nil {
		return nil, err
	}
	return &Deps{
		Logger:          logger,
		Cfg:             cfg,
		Repo:            repo,
		Users:           userCache,
		Authz:           az,
		Engine:          engine,
		Hub:             hubPID,
		sealer:          sealer,
		recoveryKey:     secure.DeriveKey(key, "recovery-codes"),
		sessions:        cache.New[string, *models.Principal](cfg.SessionCacheSize, cfg.SessionCacheTTL),
		revokedSessions: cache.New[string, struct{}](cfg.SessionCacheSize, cfg.SessionCacheTTL+cfg.OpTimeout),
		failures:        window.New(cfg.LoginFailures, cfg.LoginWindow),
		accountFailures: window.New(cfg.LoginFailures*accountFailureFactor, cfg.LoginWindow),
		totp:            keylock.New(),
	}, nil
}

// Cached returns the principal of a session's or a bot's token without a
// lookup when one is cached, for the hot path of every request.
func (d *Deps) Cached(token string) (*models.Principal, bool) {
	if !secure.ValidCredential(token) {
		return nil, false
	}
	p, ok := d.sessions.Get(string(secure.Hash(token)))
	if !ok || !time.Now().Before(p.ExpiresAt) {
		return nil, false
	}
	return p, true
}

func Pool(d *Deps) actor.Producer {
	return actors.NewPool(d.Cfg.Workers, func() actor.Receiver { return &worker{Deps: d} },
		actor.WithMiddleware(actors.Recover(d.Logger)))
}

type worker struct{ *Deps }

func (w *worker) Receive(c *actor.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	defer cancel()
	var (
		out any
		err error
	)
	switch msg := c.Message().(type) {
	case SignupInfo:
		out, err = w.signupInfo(ctx, msg)
	case SignUp:
		out, err = w.signUp(ctx, msg)
	case Login:
		out, err = w.login(ctx, msg)
	case VerifyMFA:
		out, err = w.verifyMFA(ctx, msg)
	case Authenticate:
		out, err = w.authenticate(ctx, msg)
	case Logout:
		out, err = w.logout(ctx, msg.P)
	case ChangePassword:
		out, err = w.changePassword(ctx, msg)
	case BeginTOTP:
		out, err = lockTOTP(ctx, w, msg.P, msg, w.beginTOTP)
	case ConfirmTOTP:
		out, err = lockTOTP(ctx, w, msg.P, msg, w.confirmTOTP)
	case DisableTOTP:
		out, err = lockTOTP(ctx, w, msg.P, msg, w.disableTOTP)
	case ListSessions:
		out, err = w.listSessions(ctx, msg)
	case RevokeSession:
		out, err = w.revokeSession(ctx, msg)
	case RevokeSessions:
		out, err = w.revokeSessions(ctx, msg)
	case FindUser:
		out, err = w.findUser(ctx, msg)
	case SetDeactivated:
		out, err = w.setDeactivated(ctx, msg)
	default:
		return
	}
	if err != nil {
		if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
			w.Logger.Error("accounts request failed", e.Fields()...)
		}
		actors.Reply(c, err)
		return
	}
	actors.Reply(c, out)
}

func (w *worker) validInvite(ctx context.Context, token string) (*models.Invite, error) {
	return invites.Valid(ctx, w.Repo, w.Authz, w.Users, token, gocql.UUID{})
}

func (w *worker) signupInfo(ctx context.Context, msg SignupInfo) (*SignupInfoResult, error) {
	bootstrapped, err := w.Repo.Bootstrapped(ctx)
	if err != nil {
		return nil, err
	}
	out := &SignupInfoResult{
		Bootstrap:          !bootstrapped,
		SetupTokenRequired: !bootstrapped && w.Cfg.SetupToken != "",
		SetupClosed:        !bootstrapped && w.setupClosed(),
		Open:               w.Cfg.OpenSignup,
	}
	if msg.InviteToken != "" {
		if inv, err := w.validInvite(ctx, msg.InviteToken); err == nil {
			if team, err := w.Repo.GetTeam(ctx, inv.TeamID); err == nil {
				out.InviteValid = true
				out.TeamDisplayName = team.DisplayName
			}
		}
	}
	return out, nil
}

func (w *worker) signUp(ctx context.Context, msg SignUp) (*SignUpResult, error) {
	username, err := validate.Username(msg.Username)
	if err != nil {
		return nil, err
	}
	email, err := validate.Email(msg.Email)
	if err != nil {
		return nil, err
	}
	displayName := msg.DisplayName
	if strings.TrimSpace(displayName) == "" {
		displayName = username
	}
	if displayName, err = validate.Line("display name", displayName, 1, 64); err != nil {
		return nil, err
	}
	if err := validate.Password(msg.Password, w.Cfg.MinPasswordLength); err != nil {
		return nil, err
	}

	var inv *models.Invite
	if msg.InviteToken != "" {
		if inv, err = w.validInvite(ctx, msg.InviteToken); err != nil {
			return nil, err
		}
	}
	bootstrapped, err := w.Repo.Bootstrapped(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	user := &models.User{
		ID:          ids.New(),
		Username:    username,
		Email:       email,
		DisplayName: displayName,
		Role:        models.SystemRoleUser,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	claimed := false
	if !bootstrapped && inv == nil {
		if w.setupClosed() {
			return nil, errSetupClosed
		}
		if w.Cfg.SetupToken != "" && !secure.Equal(msg.SetupToken, w.Cfg.SetupToken) {
			return nil, ierrors.New(ierrors.CodePermissionDenied, "the setup token is not correct")
		}
		if claimed, err = w.Repo.ClaimBootstrap(ctx, user.ID); err != nil {
			return nil, err
		}
		if claimed {
			user.Role = models.SystemRoleAdmin
		}
	}
	if !claimed && inv == nil && !w.Cfg.OpenSignup {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "signing up requires an invite")
	}
	release := func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if claimed {
			_ = w.Repo.ReleaseBootstrap(c, user.ID)
		}
		if inv != nil {
			_ = w.Repo.ReleaseInvite(c, inv, user.ID, time.Now())
		}
	}

	user.PasswordHash = password.Hash(msg.Password, w.Cfg.Password)
	if inv != nil {
		ok, err := w.Repo.ConsumeInvite(ctx, inv, user.ID, now)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ierrors.NotFound("invite is invalid or expired")
		}
	}
	if err := w.Repo.CreateUser(ctx, user); err != nil {
		release()
		return nil, err
	}
	sess, err := w.newSession(ctx, user, msg.IP, msg.UserAgent)
	if err != nil {
		return nil, err
	}
	w.Logger.Info("account created", zap.String("user", user.ID.String()), zap.String("role", string(user.Role)))
	out := &SignUpResult{Session: sess, User: user}
	if inv != nil {
		out.InviteTeamID, out.InviteHash = inv.TeamID, inv.TokenHash
	}
	return out, nil
}

var errSetupClosed = ierrors.FailedPrecondition("the first account needs a setup token: set services.accounts.setup_token on the server")

// setupClosed reports whether the first account may not be created: without
// a setup token, whoever reaches the server first would become its admin.
func (d *Deps) setupClosed() bool {
	return d.Cfg.SetupToken == "" && !d.Cfg.AllowOpenBootstrap
}

// WarnIfSetupClosed logs how to create the first account when none can be.
func (d *Deps) WarnIfSetupClosed(ctx context.Context) {
	if !d.setupClosed() {
		return
	}
	if done, err := d.Repo.Bootstrapped(ctx); err == nil && !done {
		d.Logger.Warn("no account exists and no setup token is set: set services.accounts.setup_token to create the first admin")
	}
}

func (w *worker) newSession(ctx context.Context, user *models.User, ip, ua string) (*pb.Session, error) {
	token := secure.Token()
	now := time.Now().UTC()
	s := &models.Session{
		TokenHash: secure.Hash(token),
		UserID:    user.ID,
		CreatedAt: now,
		ExpiresAt: now.Add(w.Cfg.SessionTTL),
		IP:        truncate(ip, 64),
		UserAgent: truncate(ua, 256),
	}
	if err := w.Repo.CreateSession(ctx, s); err != nil {
		return nil, err
	}
	return &pb.Session{Token: token, Id: secure.PublicID(s.TokenHash), ExpiresAt: convert.Time(s.ExpiresAt)}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var (
	errBadCredentials = ierrors.New(ierrors.CodeUnauthenticated, "invalid username or password")
	errTooMany        = ierrors.ResourceExhausted("too many failed attempts, try again later")
)

// addressKey groups IPv6 addresses by their /64, which one host can rotate
// through.
func addressKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	if addr.Is6() && !addr.Is4In6() {
		p, _ := addr.Prefix(64)
		return p.String()
	}
	return addr.Unmap().String()
}

func (d *Deps) recoveryHash(userID gocql.UUID, code string) []byte {
	m := hmac.New(sha256.New, d.recoveryKey)
	m.Write(userID[:])
	m.Write([]byte(code))
	return m.Sum(nil)
}

func (w *worker) login(ctx context.Context, msg Login) (*LoginResult, error) {
	login := strings.ToLower(strings.TrimSpace(msg.Login))
	if login == "" || len(login) > 254 || len(msg.Password) > 256 {
		return nil, errBadCredentials
	}
	// One address locks only itself out of an account; the account's own
	// limit, higher, bounds guessing from many addresses.
	key := "login:" + login + "|" + addressKey(msg.IP)
	if w.failures.Blocked(key) {
		return nil, errTooMany
	}
	var (
		id  gocql.UUID
		err error
	)
	if strings.Contains(login, "@") {
		id, err = w.Repo.GetUserIDByEmail(ctx, login)
	} else {
		id, err = w.Repo.GetUserIDByUsername(ctx, login)
	}
	if err != nil {
		if !ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, err
		}
		password.Dummy(msg.Password, w.Cfg.Password)
		w.failures.Add(key)
		return nil, errBadCredentials
	}
	accountKey := id.String()
	if w.accountFailures.Blocked(accountKey) {
		return nil, errTooMany
	}
	user, err := w.Repo.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	ok, rehash, err := password.Verify(msg.Password, user.PasswordHash, w.Cfg.Password)
	if err != nil {
		return nil, ierrors.Internal(err)
	}
	if !ok {
		w.failures.Add(key)
		w.accountFailures.Add(accountKey)
		return nil, errBadCredentials
	}
	w.failures.Reset(key)
	if user.Deactivated {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "account is deactivated")
	}
	if rehash {
		if err := w.Repo.UpdatePassword(ctx, user.ID, password.Hash(msg.Password, w.Cfg.Password), time.Now().UTC()); err != nil {
			w.Logger.Warn("failed to rehash password", zap.Error(err))
		}
	}
	if user.TOTPEnabled {
		token := secure.Token()
		now := time.Now().UTC()
		ch := &models.MFAChallenge{
			TokenHash: secure.Hash(token),
			UserID:    user.ID,
			ExpiresAt: now.Add(w.Cfg.MFATTL),
			IP:        truncate(msg.IP, 64),
			UserAgent: truncate(msg.UserAgent, 256),
		}
		if err := w.Repo.CreateMFAChallenge(ctx, ch, now); err != nil {
			return nil, err
		}
		return &LoginResult{MFAToken: token}, nil
	}
	sess, err := w.newSession(ctx, user, msg.IP, msg.UserAgent)
	if err != nil {
		return nil, err
	}
	return &LoginResult{Session: sess}, nil
}

// errBadChallenge has a code of its own, apart from a wrong code's
// unauthenticated, so clients tell the two apart without reading the text.
var errBadChallenge = ierrors.New(ierrors.CodeFailedPrecondition, "the sign-in attempt expired, sign in again")

func (w *worker) verifyMFA(ctx context.Context, msg VerifyMFA) (*LoginResult, error) {
	if !secure.ValidToken(msg.Token) {
		return nil, errBadChallenge
	}
	hash := secure.Hash(msg.Token)
	ch, err := w.Repo.GetMFAChallenge(ctx, hash)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, errBadChallenge
		}
		return nil, err
	}
	now := time.Now().UTC()
	if !now.Before(ch.ExpiresAt) || ch.Attempts >= w.Cfg.MFAMaxAttempts {
		_, _ = w.Repo.ConsumeMFAChallenge(ctx, hash)
		return nil, errBadChallenge
	}
	counted, err := w.Repo.CountMFAAttempt(ctx, ch, now)
	if err != nil {
		return nil, err
	}
	if !counted {
		return nil, ierrors.New(ierrors.CodeUnauthenticated, "invalid code")
	}
	// Every sign-in opens a new challenge, so wrong codes are also counted
	// per account across challenges.
	mfaKey := "mfa:" + ch.UserID.String()
	if w.failures.Blocked(mfaKey) {
		_, _ = w.Repo.ConsumeMFAChallenge(ctx, hash)
		return nil, errTooMany
	}
	user, err := w.Repo.GetUser(ctx, ch.UserID)
	if err != nil {
		return nil, err
	}
	if user.Deactivated || !user.TOTPEnabled {
		_, _ = w.Repo.ConsumeMFAChallenge(ctx, hash)
		return nil, errBadChallenge
	}
	ok, err := w.checkSecondFactor(ctx, user, msg.Code, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		w.failures.Add(mfaKey)
		w.Logger.Warn("wrong second factor", zap.String("user", user.ID.String()), zap.String("ip", msg.IP))
		return nil, ierrors.New(ierrors.CodeUnauthenticated, "invalid code")
	}
	w.failures.Reset(mfaKey)
	consumed, err := w.Repo.ConsumeMFAChallenge(ctx, hash)
	if err != nil {
		return nil, err
	}
	if !consumed {
		return nil, errBadChallenge
	}
	sess, err := w.newSession(ctx, user, msg.IP, msg.UserAgent)
	if err != nil {
		return nil, err
	}
	return &LoginResult{Session: sess}, nil
}

// checkSecondFactor accepts a current TOTP code, once, or an unused recovery
// code, which it spends.
func (w *worker) checkSecondFactor(ctx context.Context, user *models.User, code string, now time.Time) (bool, error) {
	if rc, ok := totp.NormalizeRecovery(code); ok && len(strings.TrimSpace(code)) > 6 {
		return w.Repo.ConsumeRecoveryCode(ctx, user.ID, w.recoveryHash(user.ID, rc))
	}
	secret, err := w.sealer.Open(user.TOTPSecret, user.ID[:])
	if err != nil {
		return false, ierrors.Internal(err)
	}
	step, ok := totp.Match(string(secret), code, now)
	if !ok || step <= user.TOTPLastStep {
		return false, nil
	}
	return w.Repo.AdvanceTOTPStep(ctx, user.ID, step)
}

func (w *worker) authenticate(ctx context.Context, msg Authenticate) (*models.Principal, error) {
	if !secure.ValidCredential(msg.Token) {
		return nil, ierrors.Unauthenticated()
	}
	hash := secure.Hash(msg.Token)
	now := time.Now()
	if p, ok := w.sessions.Get(string(hash)); ok && now.Before(p.ExpiresAt) {
		return p, nil
	}
	if secure.IsBotToken(msg.Token) {
		return w.authenticateBot(ctx, hash, now)
	}
	s, err := w.Repo.GetSession(ctx, hash)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, ierrors.Unauthenticated()
		}
		return nil, err
	}
	if !now.Before(s.ExpiresAt) {
		return nil, ierrors.Unauthenticated()
	}
	if t := w.Cfg.SessionIdleTimeout; t > 0 && now.Sub(lastActive(s)) > t {
		if err := w.Repo.DeleteSession(ctx, hash, s.UserID); err != nil {
			w.Logger.Warn("failed to end an idle session", zap.Error(err))
		}
		w.revoked(s.UserID, [][]byte{hash}, false)
		return nil, ierrors.Unauthenticated()
	}
	user, err := w.Repo.GetUser(ctx, s.UserID)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, ierrors.Unauthenticated()
		}
		return nil, err
	}
	if user.Deactivated {
		return nil, ierrors.Unauthenticated()
	}
	w.touch(ctx, s, now, msg.IP)
	p := &models.Principal{
		UserID:      user.ID,
		Role:        user.Role,
		SessionHash: hash,
		SessionID:   secure.PublicID(hash),
		ExpiresAt:   s.ExpiresAt,
	}
	w.sessions.SetUntil(string(hash), p, s.ExpiresAt)
	if _, ok := w.revokedSessions.Get(string(hash)); ok {
		w.sessions.Delete(string(hash))
		return nil, ierrors.Unauthenticated()
	}
	return p, nil
}

// botSession is how long a bot's principal lasts: its sockets reconnect
// then, and see a deactivation of the bot's owner at the latest.
const botSession = 24 * time.Hour

// authenticateBot resolves a bot's token. A bot works while it and its
// owner are active.
func (w *worker) authenticateBot(ctx context.Context, hash []byte, now time.Time) (*models.Principal, error) {
	id, err := w.Repo.BotOfToken(ctx, hash)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, ierrors.Unauthenticated()
		}
		return nil, err
	}
	bot, err := w.Repo.GetUser(ctx, id)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, ierrors.Unauthenticated()
		}
		return nil, err
	}
	if bot.Deactivated || !bot.Bot {
		return nil, ierrors.Unauthenticated()
	}
	owner, err := w.Repo.GetUser(ctx, bot.BotOwner)
	if err != nil && !ierrors.Is(err, ierrors.CodeNotFound) {
		return nil, err
	}
	if owner == nil || owner.Deactivated {
		return nil, ierrors.Unauthenticated()
	}
	p := &models.Principal{
		UserID:      bot.ID,
		Role:        models.SystemRoleUser,
		SessionHash: hash,
		SessionID:   secure.PublicID(hash),
		ExpiresAt:   now.Add(botSession),
		Bot:         true,
	}
	w.sessions.SetUntil(string(hash), p, p.ExpiresAt)
	if _, ok := w.revokedSessions.Get(string(hash)); ok {
		w.sessions.Delete(string(hash))
		return nil, ierrors.Unauthenticated()
	}
	return p, nil
}

// touch records that s is in use, at most every LastSeenInterval unless it
// moved to another address.
func (w *worker) touch(ctx context.Context, s *models.Session, now time.Time, ip string) {
	ip = truncate(ip, 64)
	if ip == "" {
		ip = s.LastIP
	}
	if now.Sub(s.LastSeen) < w.Cfg.LastSeenInterval && ip == s.LastIP {
		return
	}
	if err := w.Repo.TouchSession(ctx, s, now.UTC(), ip); err != nil {
		w.Logger.Warn("failed to record a session's use", zap.Error(err))
	}
}

func (w *worker) logout(ctx context.Context, p *models.Principal) (Done, error) {
	if err := w.Repo.DeleteSession(ctx, p.SessionHash, p.UserID); err != nil {
		return Done{}, err
	}
	w.sessions.Delete(string(p.SessionHash))
	w.revoked(p.UserID, [][]byte{p.SessionHash}, false)
	return Done{}, nil
}

func (w *worker) revoked(userID gocql.UUID, hashes [][]byte, all bool) {
	w.Revoked(userID, hashes, all)
}

// Revoked forgets revoked sessions, or bot tokens, and closes the
// connections they opened, or with all every connection of the user.
func (d *Deps) Revoked(userID gocql.UUID, hashes [][]byte, all bool) {
	if len(hashes) == 0 && !all {
		return
	}
	idList := make([]string, len(hashes))
	for i, h := range hashes {
		d.revokedSessions.Set(string(h), struct{}{})
		d.sessions.Delete(string(h))
		idList[i] = secure.PublicID(h)
	}
	d.Engine.Send(d.Hub, hub.RevokeSessions{UserID: userID.String(), SessionIDs: idList, All: all})
	if d.Voice != nil {
		d.Engine.Send(d.Voice, voice.RevokeSessions{UserID: userID, SessionIDs: idList, All: all})
	}
}

func (w *worker) verifyPassword(ctx context.Context, p *models.Principal, pass string) (*models.User, error) {
	key := "reauth:" + p.UserID.String()
	if w.failures.Blocked(key) {
		return nil, ierrors.ResourceExhausted("too many failed attempts, try again later")
	}
	user, err := w.Repo.GetUser(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if len(pass) > 256 {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "password is incorrect")
	}
	ok, _, err := password.Verify(pass, user.PasswordHash, w.Cfg.Password)
	if err != nil {
		return nil, ierrors.Internal(err)
	}
	if !ok {
		w.failures.Add(key)
		return nil, ierrors.New(ierrors.CodePermissionDenied, "password is incorrect")
	}
	return user, nil
}

func (w *worker) changePassword(ctx context.Context, msg ChangePassword) (Done, error) {
	if err := validate.Password(msg.NewPassword, w.Cfg.MinPasswordLength); err != nil {
		return Done{}, err
	}
	user, err := w.verifyPassword(ctx, msg.P, msg.Current)
	if err != nil {
		return Done{}, err
	}
	if err := w.Repo.UpdatePassword(ctx, user.ID, password.Hash(msg.NewPassword, w.Cfg.Password), time.Now().UTC()); err != nil {
		return Done{}, err
	}
	revoked, err := w.Repo.DeleteUserSessions(ctx, user.ID, msg.P.SessionHash)
	w.revoked(user.ID, revoked, false)
	if err != nil {
		return Done{}, err
	}
	return Done{}, nil
}

// lockTOTP runs fn holding the caller's two-factor settings.
func lockTOTP[M, T any](ctx context.Context, w *worker, p *models.Principal, msg M, fn func(context.Context, M) (T, error)) (T, error) {
	unlock, err := w.totp.Lock(ctx, p.UserID.String())
	if err != nil {
		var zero T
		return zero, ierrors.Unavailable("busy, try again")
	}
	defer unlock()
	return fn(ctx, msg)
}

func (w *worker) beginTOTP(ctx context.Context, msg BeginTOTP) (*TOTPSetup, error) {
	user, err := w.verifyPassword(ctx, msg.P, msg.Password)
	if err != nil {
		return nil, err
	}
	if user.TOTPEnabled {
		return nil, ierrors.FailedPrecondition("two-factor authentication is already enabled")
	}
	secret, url, err := totp.Generate(w.Cfg.TOTPIssuer, user.Username)
	if err != nil {
		return nil, ierrors.Internal(err)
	}
	if err := w.Repo.SetTOTPPending(ctx, user.ID, w.sealer.Seal([]byte(secret), user.ID[:]), time.Now().UTC()); err != nil {
		return nil, err
	}
	return &TOTPSetup{Secret: secret, URL: url}, nil
}

func (w *worker) confirmTOTP(ctx context.Context, msg ConfirmTOTP) (RecoveryCodes, error) {
	key := "totp:" + msg.P.UserID.String()
	if w.failures.Blocked(key) {
		return nil, ierrors.ResourceExhausted("too many failed attempts, try again later")
	}
	user, err := w.Repo.GetUser(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	if user.TOTPEnabled {
		return nil, ierrors.FailedPrecondition("two-factor authentication is already enabled")
	}
	if len(user.TOTPPending) == 0 {
		return nil, ierrors.FailedPrecondition("start the setup first")
	}
	secret, err := w.sealer.Open(user.TOTPPending, user.ID[:])
	if err != nil {
		return nil, ierrors.Internal(err)
	}
	step, ok := totp.Match(string(secret), msg.Code, time.Now())
	if !ok {
		w.failures.Add(key)
		return nil, ierrors.InvalidArgument("invalid code")
	}
	// The codes go first: enabling without them could lock the user out.
	codes := totp.RecoveryCodes(recoveryCodeCount)
	hashes := make([][]byte, len(codes))
	for i, rc := range codes {
		hashes[i] = w.recoveryHash(user.ID, rc)
	}
	if err := w.Repo.ReplaceRecoveryCodes(ctx, user.ID, hashes); err != nil {
		return nil, err
	}
	applied, err := w.Repo.EnableTOTP(ctx, user.ID, user.TOTPPending, step, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, ierrors.FailedPrecondition("the setup changed, start again")
	}
	w.failures.Reset(key)
	w.Users.Invalidate(user.ID)
	return codes, nil
}

func (w *worker) disableTOTP(ctx context.Context, msg DisableTOTP) (Done, error) {
	user, err := w.verifyPassword(ctx, msg.P, msg.Password)
	if err != nil {
		return Done{}, err
	}
	if !user.TOTPEnabled {
		return Done{}, ierrors.FailedPrecondition("two-factor authentication is not enabled")
	}
	ok, err := w.checkSecondFactor(ctx, user, msg.Code, time.Now())
	if err != nil {
		return Done{}, err
	}
	if !ok {
		w.failures.Add("reauth:" + user.ID.String())
		return Done{}, ierrors.InvalidArgument("invalid code")
	}
	if err := w.Repo.DisableTOTP(ctx, user.ID, time.Now().UTC()); err != nil {
		return Done{}, err
	}
	w.Users.Invalidate(user.ID)
	return Done{}, nil
}
