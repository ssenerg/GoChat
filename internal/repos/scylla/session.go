package scylla

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"go.uber.org/zap"
)

func newCluster(cfg *Config, keyspace string) *gocql.ClusterConfig {
	cluster := gocql.NewCluster(cfg.Hosts...)
	cluster.Keyspace = keyspace
	cluster.Timeout = cfg.Timeout
	cluster.ConnectTimeout = cfg.ConnectTimeout
	cluster.NumConns = cfg.NumConns
	cluster.Consistency, _ = parseConsistency(cfg.Consistency)
	cluster.SerialConsistency, _ = parseSerial(cfg.SerialConsistency)
	cluster.RetryPolicy = retryPolicy{&gocql.ExponentialBackoffRetryPolicy{NumRetries: 3}}
	cluster.DisableInitialHostLookup = cfg.DisableHostLookup
	if cfg.LocalDC != "" {
		cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(gocql.DCAwareRoundRobinPolicy(cfg.LocalDC))
	} else {
		cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(gocql.RoundRobinHostPolicy())
	}
	if cfg.Username != "" {
		cluster.Authenticator = gocql.PasswordAuthenticator{Username: cfg.Username, Password: cfg.Password}
	}
	if cfg.TLS.Enabled {
		cluster.SslOpts = &gocql.SslOptions{
			CaPath:                 cfg.TLS.CAFile,
			CertPath:               cfg.TLS.CertFile,
			KeyPath:                cfg.TLS.KeyFile,
			EnableHostVerification: true,
		}
	}
	return cluster
}

// retryPolicy backs off and retries, except a conditional write that timed
// out: it may have applied, and a retry would find its own row and report
// the condition as failed.
type retryPolicy struct {
	*gocql.ExponentialBackoffRetryPolicy
}

func (p retryPolicy) GetRetryType(err error) gocql.RetryType {
	if casUnknown(err) {
		return gocql.Rethrow
	}
	return p.ExponentialBackoffRetryPolicy.GetRetryType(err)
}

func (p retryPolicy) GetRetryTypeLWT(err error) gocql.RetryType {
	if casUnknown(err) {
		return gocql.Rethrow
	}
	return p.ExponentialBackoffRetryPolicy.GetRetryTypeLWT(err)
}

func casUnknown(err error) bool {
	var wt *gocql.RequestErrWriteTimeout
	if errors.As(err, &wt) {
		return wt.WriteType == "CAS"
	}
	var cu *gocql.RequestErrCASWriteUnknown
	return errors.As(err, &cu)
}

// The pauses between attempts to connect while Scylla is not reachable yet:
// the first, doubling up to the last. Variables, so tests can shorten them.
var (
	firstRetryPause = 500 * time.Millisecond
	maxRetryPause   = 5 * time.Second
)

// connect creates a session, trying again while Scylla is not reachable yet,
// until ctx ends: when a machine boots, the server and Scylla start together,
// and Scylla takes a while to accept connections. Each attempt gets a cluster
// of its own from cluster: gocql's host policies serve one session only.
func connect(ctx context.Context, logger *zap.Logger, cluster func() *gocql.ClusterConfig) (gocqlx.Session, error) {
	pause := firstRetryPause
	for attempt := 1; ; attempt++ {
		session, err := gocqlx.WrapSession(cluster().CreateSession())
		if err == nil {
			return session, nil
		}
		if ctx.Err() != nil {
			return gocqlx.Session{}, err
		}
		logger.Warn("scylla is not reachable yet, trying again",
			zap.Int("attempt", attempt), zap.Duration("in", pause), zap.Error(err))
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return gocqlx.Session{}, err
		case <-timer.C:
		}
		pause = min(pause*2, maxRetryPause)
	}
}

// Open connects to the configured keyspace, waiting for Scylla until ctx
// ends.
func Open(ctx context.Context, logger *zap.Logger, cfg *Config) (gocqlx.Session, error) {
	session, err := connect(ctx, logger, func() *gocql.ClusterConfig { return newCluster(cfg, cfg.Keyspace) })
	if err != nil {
		logger.Error("failed to connect to scylla", zap.Error(err))
		return gocqlx.Session{}, fmt.Errorf("failed to connect to scylla: %w", err)
	}
	logger.Info("connected to scylla", zap.Strings("hosts", cfg.Hosts), zap.String("keyspace", cfg.Keyspace))
	if cfg.LocalDC == "" {
		logger.Warn("local_dc is not set: in a cluster of several datacenters, local consistency levels will span them")
	}
	return session, nil
}

// EnsureKeyspace creates the keyspace when it is missing, waiting for Scylla
// until ctx ends.
func EnsureKeyspace(ctx context.Context, logger *zap.Logger, cfg *Config) error {
	session, err := connect(ctx, logger, func() *gocql.ClusterConfig { return newCluster(cfg, "") })
	if err != nil {
		return fmt.Errorf("failed to connect to scylla: %w", err)
	}
	defer session.Close()
	// Keyspace is validated as an identifier and replication as a map literal;
	// neither can be bound as a parameter in DDL.
	stmt := fmt.Sprintf("CREATE KEYSPACE IF NOT EXISTS %s WITH replication = %s", cfg.Keyspace, cfg.Replication)
	if err := session.ExecStmt(stmt); err != nil {
		return fmt.Errorf("failed to create keyspace: %w", err)
	}
	logger.Info("keyspace ready", zap.String("keyspace", cfg.Keyspace))
	return nil
}
