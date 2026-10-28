package channels

import (
	"context"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// noChannels finds no channel.
type noChannels struct{ Repo }

func (noChannels) GetChannel(context.Context, gocql.UUID) (*models.Channel, error) {
	return nil, ierrors.NotFound("no channel")
}

// TestManagerStopsAsAChannelPassivates stops the manager just as a channel
// asks to stop, which once left the manager waiting on that channel forever.
func TestManagerStopsAsAChannelPassivates(t *testing.T) {
	for range 300 {
		engine, err := actor.NewEngine(actor.NewEngineConfig())
		if err != nil {
			t.Fatal(err)
		}
		m := engine.Spawn(New(&Deps{
			Logger: zap.NewNop(), Cfg: &Config{IdleTimeout: time.Minute, OpTimeout: time.Second}, Repo: noChannels{}, Engine: engine,
		}), "channels", actor.WithID("main"))
		// The channel is not found, so it asks to stop at once.
		id := ids.New()
		_, err = actors.Ask[*AccessResult](context.Background(), engine, m, Access{Target{ChannelID: id}})
		if !ierrors.Is(err, ierrors.CodeNotFound) {
			t.Fatalf("access: %v", err)
		}
		select {
		case <-engine.Poison(m).Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the manager never stopped")
		}
		if engine.Registry.GetPID("channels/main/channel", id.String()+".1") != nil {
			t.Fatal("the channel outlived the manager")
		}
	}
}
