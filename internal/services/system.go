// Package services assembles the chat server's actors.
package services

import (
	"context"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/repos/minio"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/accounts"
	"github.com/ssenerg/gochat/internal/services/blocks"
	"github.com/ssenerg/gochat/internal/services/bots"
	"github.com/ssenerg/gochat/internal/services/callbacks"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/directory"
	"github.com/ssenerg/gochat/internal/services/files"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/index"
	"github.com/ssenerg/gochat/internal/services/previews"
	"github.com/ssenerg/gochat/internal/services/push"
	"github.com/ssenerg/gochat/internal/services/safety"
	"github.com/ssenerg/gochat/internal/services/updates"
	"github.com/ssenerg/gochat/internal/services/users"
	"github.com/ssenerg/gochat/internal/services/voice"
)

type System struct {
	logger   *zap.Logger
	Engine   *actor.Engine
	Hub      *actor.PID
	Channels *actor.PID
	Accounts *actor.PID
	// Auth resolves session tokens apart from Accounts, so a flood of
	// sign-ins, each hashing a password, cannot hold up signed-in requests.
	Auth      *actor.PID
	Directory *actor.PID
	Files     *actor.PID
	Search    *actor.PID
	Voice     *actor.PID
	// Updates numbers what happens to each account.
	Updates *actor.PID
	Bots    *actor.PID
	// Push reaches closed apps; nil without a provider.
	Push *actor.PID
	// Safety blocks people and takes reports.
	Safety *actor.PID
	// Previews makes link previews; nil while they are off.
	Previews *previews.Service
	Sessions *accounts.Deps
	Users    *users.Cache
	janitor  *actor.PID
	filing   *actor.PID
	backfill *actor.PID
	adopter  *actor.PID
	events   *actor.PID
	// callbacks holds the presses bots have yet to answer, webhooks
	// delivers the updates of bots that have one.
	callbacks *actor.PID
	webhooks  *actor.PID
}

func Start(logger *zap.Logger, cfg *Config, repo *scylla.Repository, storage *minio.Storage, az *authz.Authorizer) (*System, error) {
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		return nil, err
	}
	s := &System{logger: logger, Engine: engine}
	s.Users = users.NewCache(repo, cfg.Users.CacheSize, cfg.Users.CacheTTL)
	userCache := s.Users
	blocked := blocks.NewCache(repo, cfg.Safety.MaxBlocks, cfg.Users.CacheSize, cfg.Users.CacheTTL)
	s.events = actors.LogEvents(engine, logger.Named("actors"))

	// Stateful actors survive a panic in one message instead of restarting
	// with their state lost.
	recovered := actor.WithMiddleware(actors.Recover(logger))
	s.Hub = engine.Spawn(hub.New(logger.Named("hub"), cfg.Hub), "hub", actor.WithID("main"), recovered)
	indexDeps := &index.Deps{Logger: logger.Named("search"), Cfg: cfg.Search, Repo: repo, Users: userCache}
	s.filing = engine.Spawn(index.Writer(indexDeps), "search", actor.WithID("writer"), recovered)
	s.Search = engine.Spawn(index.Searcher(indexDeps), "search", actor.WithID("main"))
	if cfg.Search.Backfill {
		s.backfill = engine.Spawn(index.Backfill(indexDeps), "search", actor.WithID("backfill"), recovered)
	}
	s.Voice = engine.Spawn(voice.New(&voice.Deps{
		Logger: logger.Named("voice"), Cfg: cfg.Voice, Repo: repo, Engine: engine, Hub: s.Hub,
	}), "voice", actor.WithID("main"), recovered)
	providers, err := push.Providers(cfg.Push, push.Client())
	if err != nil {
		return nil, err
	}
	if len(providers) > 0 {
		s.Push = engine.Spawn(push.Pool(&push.Deps{
			Logger: logger.Named("push"), Cfg: cfg.Push, Repo: repo, Users: userCache, Engine: engine, Hub: s.Hub,
			Providers: providers, IdleTimeout: cfg.Accounts.SessionIdleTimeout, Blocks: blocked,
		}), "push", actor.WithID("main"))
		logger.Info("push notifications on", zap.Bool("fcm", providers[models.PushFCM] != nil), zap.Bool("apns", providers[models.PushAPNs] != nil))
	}
	channelDeps := &channels.Deps{
		Logger: logger.Named("channels"), Cfg: cfg.Channels, Repo: repo, Storage: storage,
		Authz: az, Users: userCache, Engine: engine, Hub: s.Hub, Search: s.filing, UpdatesCfg: cfg.Updates,
		Push: s.Push, Blocks: blocked,
	}
	s.Updates = engine.Spawn(updates.New(&updates.Deps{
		Logger: logger.Named("updates"), Cfg: cfg.Updates, Repo: repo, Engine: engine, Hub: s.Hub,
		Render: channels.Render(channelDeps), Users: userCache,
	}), "updates", actor.WithID("main"), recovered)
	s.callbacks = engine.Spawn(callbacks.New(s.Hub), "callbacks", actor.WithID("main"), recovered)
	channelDeps.Updates, channelDeps.Callbacks = s.Updates, s.callbacks
	s.Channels = engine.Spawn(channels.New(channelDeps), "channels", actor.WithID("main"), recovered)
	if cfg.Previews.Enabled {
		s.Previews = previews.Start(&previews.Deps{
			Logger: logger.Named("previews"), Cfg: cfg.Previews, Repo: repo, Storage: storage, Engine: engine,
			Channels: s.Channels, Fetcher: previews.Fetcher(cfg.Previews),
		})
		channelDeps.Previews = s.Previews
	}
	fileDeps := &files.Deps{
		Logger: logger.Named("files"), Cfg: cfg.Files, Repo: repo, Storage: storage,
		Authz: az, Engine: engine, Channels: s.Channels,
	}
	s.Files = engine.Spawn(files.Pool(fileDeps), "files", actor.WithID("main"))
	s.janitor = engine.Spawn(files.Janitor(fileDeps), "janitor", actor.WithID("main"), recovered)
	directoryDeps := &directory.Deps{
		Logger: logger.Named("directory"), Cfg: cfg.Directory, Repo: repo, Storage: storage, Authz: az,
		Users: userCache, Engine: engine, Hub: s.Hub, Channels: s.Channels, Voice: s.Voice, Updates: s.Updates,
		Blocks: blocked,
	}
	s.Directory = engine.Spawn(directory.Pool(directoryDeps), "directory", actor.WithID("main"))
	s.adopter = engine.Spawn(directory.Adopt(directoryDeps), "directory", actor.WithID("adopt"), recovered)
	accountDeps, err := accounts.NewDeps(logger.Named("accounts"), cfg.Accounts, repo, userCache, az, engine, s.Hub)
	if err != nil {
		return nil, err
	}
	accountDeps.Voice, accountDeps.Push = s.Voice, s.Push
	s.Sessions = accountDeps
	checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	accountDeps.WarnIfSetupClosed(checkCtx)
	cancel()
	s.Accounts = engine.Spawn(accounts.Pool(accountDeps), "accounts", actor.WithID("main"))
	s.Auth = engine.Spawn(accounts.Pool(accountDeps), "accounts", actor.WithID("auth"))
	botDeps := &bots.Deps{
		Logger: logger.Named("bots"), Cfg: cfg.Bots, Repo: repo, Users: userCache, Engine: engine,
		Directory: s.Directory, Channels: s.Channels, Callbacks: s.callbacks, Revoke: accountDeps.Revoked,
	}
	s.webhooks = engine.Spawn(bots.Webhooks(&bots.WebhookDeps{
		Deps: botDeps, Updates: s.Updates, Client: bots.Client(cfg.Bots.WebhookTimeout, cfg.Bots.AllowPrivateWebhooks),
	}), "webhooks", actor.WithID("main"), recovered)
	botDeps.Webhooks = s.webhooks
	s.Bots = engine.Spawn(bots.Pool(botDeps), "bots", actor.WithID("main"))
	s.Safety = engine.Spawn(safety.Pool(&safety.Deps{
		Logger: logger.Named("safety"), Cfg: cfg.Safety, Repo: repo, Authz: az, Users: userCache, Blocks: blocked,
		Engine: engine, Hub: s.Hub, Updates: s.Updates, Channels: s.Channels, Directory: s.Directory, Accounts: s.Accounts,
	}), "safety", actor.WithID("main"))
	logger.Info("actors started")
	return s, nil
}

// Stop drains the actors, request handlers first.
func (s *System) Stop(timeout time.Duration) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var previewer *actor.PID
	if s.Previews != nil {
		previewer = s.Previews.PID
	}
	for _, pid := range []*actor.PID{s.Safety, s.Bots, s.webhooks, s.Accounts, s.Auth, s.Directory, s.Files, s.Search, s.backfill, s.adopter, s.janitor, previewer, s.Channels, s.Push, s.Voice, s.callbacks, s.Updates, s.filing, s.Hub, s.events} {
		if pid == nil {
			continue
		}
		select {
		case <-s.Engine.PoisonCtx(ctx, pid).Done():
		case <-ctx.Done():
			s.logger.Warn("actors did not stop in time")
			return
		}
	}
	s.logger.Info("actors stopped")
}
