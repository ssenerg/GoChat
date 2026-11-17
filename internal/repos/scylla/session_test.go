package scylla

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// unreachable is a Config for an address nothing listens on, so that every
// attempt to connect fails at once.
func unreachable(t *testing.T) *Config {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return &Config{
		Hosts: []string{addr}, Keyspace: "gochat", Consistency: "local_quorum", SerialConsistency: "local_serial",
		Timeout: time.Second, ConnectTimeout: time.Second, NumConns: 1, DisableHostLookup: true, TLS: &TLSConfig{},
	}
}

func shortPauses(t *testing.T) {
	t.Helper()
	first, last := firstRetryPause, maxRetryPause
	firstRetryPause, maxRetryPause = 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { firstRetryPause, maxRetryPause = first, last })
}

func TestOpenWaitsForScyllaUntilTheContextEnds(t *testing.T) {
	shortPauses(t)
	core, logs := observer.New(zap.WarnLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := Open(ctx, zap.New(core), unreachable(t)); err == nil {
		t.Fatal("connected to an address nothing listens on")
	}
	if n := logs.FilterMessage("scylla is not reachable yet, trying again").Len(); n < 3 {
		t.Fatalf("tried again %d times, want several", n)
	}
	if took := time.Since(start); took < 500*time.Millisecond || took > 5*time.Second {
		t.Fatalf("gave up after %v, want soon after the context's 500ms", took)
	}
}

func TestOpenStopsWaitingWhenCanceled(t *testing.T) {
	shortPauses(t)
	core, logs := observer.New(zap.WarnLevel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Open(ctx, zap.New(core), unreachable(t)); err == nil {
		t.Fatal("connected to an address nothing listens on")
	}
	if n := logs.FilterMessage("scylla is not reachable yet, trying again").Len(); n != 0 {
		t.Fatalf("tried again %d times after the context was canceled", n)
	}
}

func TestEnsureKeyspaceWaitsForScylla(t *testing.T) {
	shortPauses(t)
	core, logs := observer.New(zap.WarnLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if err := EnsureKeyspace(ctx, zap.New(core), unreachable(t)); err == nil {
		t.Fatal("connected to an address nothing listens on")
	}
	if n := logs.FilterMessage("scylla is not reachable yet, trying again").Len(); n < 2 {
		t.Fatalf("tried again %d times, want several", n)
	}
}
