package accounts

import (
	"context"
	"testing"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

func (f *fakeRepo) BotOfToken(_ context.Context, hash []byte) (gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bots {
		if string(b.TokenHash) == string(hash) {
			return b.ID, nil
		}
	}
	return gocql.UUID{}, ierrors.NotFound("not found")
}

func (f *fakeRepo) ListBotIDs(_ context.Context, owner gocql.UUID) ([]gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gocql.UUID
	for _, b := range f.bots {
		if b.OwnerID == owner {
			out = append(out, b.ID)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetBot(_ context.Context, id gocql.UUID) (*models.Bot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.bots[id]; b != nil {
		return b, nil
	}
	return nil, ierrors.NotFound("not found")
}

// bot makes a bot owned by owner and returns its token.
func (f *fixture) bot(owner *models.User, name string) (*models.User, string) {
	f.t.Helper()
	token := secure.BotToken()
	u := &models.User{ID: ids.New(), Username: name, Email: name + "@bots.test", Role: models.SystemRoleUser, Bot: true, BotOwner: owner.ID}
	if err := f.repo.CreateUser(context.Background(), u); err != nil {
		f.t.Fatal(err)
	}
	f.repo.mu.Lock()
	if f.repo.bots == nil {
		f.repo.bots = map[gocql.UUID]*models.Bot{}
	}
	f.repo.bots[u.ID] = &models.Bot{ID: u.ID, OwnerID: owner.ID, TokenHash: secure.Hash(token)}
	f.repo.mu.Unlock()
	return u, token
}

func TestBotSignsInWithItsToken(t *testing.T) {
	f := newFixture(t, nil)
	owner := f.user("owner", models.SystemRoleUser)
	bot, token := f.bot(owner, "helper_bot")
	p, err := ask[*models.Principal](f, Authenticate{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Bot || p.UserID != bot.ID || p.SessionID != secure.PublicID(secure.Hash(token)) {
		t.Fatalf("principal %+v", p)
	}
	if until := time.Until(p.ExpiresAt); until < 23*time.Hour || until > 25*time.Hour {
		t.Fatalf("a bot's principal lasts %v", until)
	}
	if c, ok := f.deps.Cached(token); !ok || c.UserID != bot.ID {
		t.Fatal("the bot's principal was not cached")
	}
	if _, err := ask[*models.Principal](f, Authenticate{Token: "b_" + secure.Token()}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("an unknown bot token: %v", err)
	}
}

func TestBotStopsWithItsOwner(t *testing.T) {
	f := newFixture(t, nil)
	admin := f.user("admin", models.SystemRoleAdmin)
	owner := f.user("owner", models.SystemRoleUser)
	bot, token := f.bot(owner, "helper_bot")
	if _, err := ask[*models.Principal](f, Authenticate{Token: token}); err != nil {
		t.Fatal(err)
	}
	if _, err := ask[*models.User](f, SetDeactivated{P: &models.Principal{UserID: admin.ID, Role: models.SystemRoleAdmin}, UserID: owner.ID.String(), Deactivated: true}); err != nil {
		t.Fatal(err)
	}
	closed := false
	for !closed {
		select {
		case r := <-f.hub:
			closed = r.UserID == bot.ID.String() && r.All
		case <-time.After(2 * time.Second):
			t.Fatal("the bot's sockets were not closed")
		}
	}
	if _, ok := f.deps.Cached(token); ok {
		t.Fatal("the bot's principal stayed cached")
	}
	if _, err := ask[*models.Principal](f, Authenticate{Token: token}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("a bot of a deactivated owner signed in: %v", err)
	}
}
