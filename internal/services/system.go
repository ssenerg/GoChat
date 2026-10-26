// Package services assembles the chat server's actors.
package services

import (
	"context"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/repos/minio"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/accounts"
	"github.com/ssenerg/gochat/internal/services/bots"
	"github.com/ssenerg/gochat/internal/services/callbacks"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/directory"
	"github.com/ssenerg/gochat/internal/services/files"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/index"
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
	Updates  *actor.PID
	Bots     *actor.PID
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
	channelDeps := &channels.Deps{
		Logger: logger.Named("channels"), Cfg: cfg.Channels, Repo: repo, Storage: storage,
		Authz: az, Users: userCache, Engine: engine, Hub: s.Hub, Search: s.filing, UpdatesCfg: cfg.Updates,
	}
	s.Updates = engine.Spawn(updates.New(&updates.Deps{
		Logger: logger.Named("updates"), Cfg: cfg.Updates, Repo: repo, Engine: engine, Hub: s.Hub,
		Render: channels.Render(channelDeps), Users: userCache,
	}), "updates", actor.WithID("main"), recovered)
	s.callbacks = engine.Spawn(callbacks.New(s.Hub), "callbacks", actor.WithID("main"), recovered)
	channelDeps.Updates, channelDeps.Callbacks = s.Updates, s.callbacks
	s.Channels = engine.Spawn(channels.New(channelDeps), "channels", actor.WithID("main"), recovered)
	fileDeps := &files.Deps{
		Logger: logger.Named("files"), Cfg: cfg.Files, Repo: repo, Storage: storage,
		Authz: az, Engine: engine, Channels: s.Channels,
	}
	s.Files = engine.Spawn(files.Pool(fileDeps), "files", actor.WithID("main"))
	s.janitor = engine.Spawn(files.Janitor(fileDeps), "janitor", actor.WithID("main"), recovered)
	directoryDeps := &directory.Deps{
		Logger: logger.Named("directory"), Cfg: cfg.Directory, Repo: repo, Storage: storage, Authz: az,
		Users: userCache, Engine: engine, Hub: s.Hub, Channels: s.Channels, Voice: s.Voice, Updates: s.Updates,
	}
	s.Directory = engine.Spawn(directory.Pool(directoryDeps), "directory", actor.WithID("main"))
	s.adopter = engine.Spawn(directory.Adopt(directoryDeps), "directory", actor.WithID("adopt"), recovered)
	accountDeps, err := accounts.NewDeps(logger.Named("accounts"), cfg.Accounts, repo, userCache, az, engine, s.Hub)
	if err != nil {
		return nil, err
	}
	accountDeps.Voice = s.Voice
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
	for _, pid := range []*actor.PID{s.Bots, s.webhooks, s.Accounts, s.Auth, s.Directory, s.Files, s.Search, s.backfill, s.adopter, s.janitor, s.Channels, s.Voice, s.callbacks, s.Updates, s.filing, s.Hub, s.events} {
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
