package channels

import (
	"testing"

	"github.com/gocql/gocql"
	"golang.org/x/time/rate"

	"github.com/ssenerg/gochat/internal/authz/authztest"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// Edits, deletes and pins reach every member as posts do, so they draw on
// the posting budget; one spent refuses them before any work.
func TestChangesShareThePostingBudget(t *testing.T) {
	p := &models.Principal{UserID: ids.New(), Role: models.SystemRoleUser}
	a := &channel{
		Deps:    &Deps{Cfg: &Config{PostRate: 1, PostBurst: 1, MaxMessageLength: 100}, Authz: authztest.New(t)},
		ch:      &models.Channel{Kind: models.ChannelPublic},
		members: map[gocql.UUID]models.ChannelRole{p.UserID: models.ChannelRoleMember},
		posters: map[gocql.UUID]*rate.Limiter{},
	}
	spend := func() {
		for a.poster(p.UserID).Allow() {
		}
	}
	target := Target{P: p, ChannelID: ids.New()}
	spend()
	if _, err := a.edit(Edit{Target: target, MessageID: ids.NewTime().String(), Content: "x"}); !ierrors.Is(err, ierrors.CodeResourceExhausted) {
		t.Errorf("edit: %v", err)
	}
	spend()
	if _, err := a.delete(Delete{Target: target, MessageID: ids.NewTime().String()}); !ierrors.Is(err, ierrors.CodeResourceExhausted) {
		t.Errorf("delete: %v", err)
	}
	spend()
	if _, err := a.pin(Pin{Target: target, MessageID: ids.NewTime().String(), Pinned: true}); !ierrors.Is(err, ierrors.CodeResourceExhausted) {
		t.Errorf("pin: %v", err)
	}
}
