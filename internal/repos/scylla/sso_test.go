package scylla

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

func TestSSOLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	f := &models.SSOFlow{StateHash: secure.Hash(secure.Token()), Provider: "corp", Nonce: "n", Verifier: []byte{1, 2}, RedirectURI: "https://chat.example.test/cb", ExpiresAt: now.Add(time.Minute)}
	if err := r.CreateSSOFlow(ctx, f, now); err != nil {
		t.Fatal(err)
	}
	got, err := r.ConsumeSSOFlow(ctx, f.StateHash)
	if err != nil || got.Provider != "corp" || got.Nonce != "n" || string(got.Verifier) != "\x01\x02" || !ids.IsZero(got.LinkUser) || got.AppChallenge != "" || !got.ExpiresAt.Equal(f.ExpiresAt) {
		t.Fatalf("flow %+v, %v", got, err)
	}
	if _, err := r.ConsumeSSOFlow(ctx, f.StateHash); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a flow was consumed twice: %v", err)
	}
	linker := ids.New()
	f2 := &models.SSOFlow{StateHash: secure.Hash(secure.Token()), Provider: "corp", LinkUser: linker, AppChallenge: "c", ExpiresAt: now.Add(time.Minute)}
	if err := r.CreateSSOFlow(ctx, f2, now); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ConsumeSSOFlow(ctx, f2.StateHash); err != nil || got.LinkUser != linker || got.AppChallenge != "c" {
		t.Fatalf("flow %+v, %v", got, err)
	}

	tk := &models.SSOTicket{TokenHash: secure.Hash(secure.Token()), Kind: models.SSOTicketLink, Provider: "corp", Issuer: "https://id.test", Subject: "s", Email: "a@x.test", ExpiresAt: now.Add(time.Minute)}
	if err := r.CreateSSOTicket(ctx, tk, now); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetSSOTicket(ctx, tk.TokenHash); err != nil || got.Kind != models.SSOTicketLink || got.Subject != "s" || !ids.IsZero(got.UserID) || got.Challenge != "" {
		t.Fatalf("ticket %+v, %v", got, err)
	}
	if ok, err := r.ConsumeSSOTicket(ctx, tk.TokenHash); err != nil || !ok {
		t.Fatalf("consume: %v %v", ok, err)
	}
	if ok, err := r.ConsumeSSOTicket(ctx, tk.TokenHash); err != nil || ok {
		t.Fatalf("a ticket was consumed twice: %v %v", ok, err)
	}

	issuer := "https://id.test/" + ids.New().String()
	a, b := ids.New(), ids.New()
	id := &models.SSOIdentity{Issuer: issuer, Subject: "s1", UserID: a, Provider: "corp", Email: "a@x.test", LinkedAt: now}
	if ok, err := r.LinkSSOIdentity(ctx, id); err != nil || !ok {
		t.Fatalf("link: %v %v", ok, err)
	}
	if ok, err := r.LinkSSOIdentity(ctx, &models.SSOIdentity{Issuer: issuer, Subject: "s1", UserID: b, Provider: "corp", LinkedAt: now}); err != nil || ok {
		t.Fatalf("someone else's identity was linked: %v %v", ok, err)
	}
	if ok, err := r.LinkSSOIdentity(ctx, &models.SSOIdentity{Issuer: issuer, Subject: "s2", UserID: b, Provider: "corp", LinkedAt: now}); err != nil || !ok {
		t.Fatalf("link b: %v %v", ok, err)
	}
	if owner, err := r.GetSSOIdentity(ctx, issuer, "s1"); err != nil || owner != a {
		t.Fatalf("owner %v, %v", owner, err)
	}
	if list, err := r.ListUserSSOIdentities(ctx, a); err != nil || len(list) != 1 || list[0].Email != "a@x.test" || !list[0].LinkedAt.Equal(now) {
		t.Fatalf("a's identities %+v, %v", list, err)
	}
	first, err := r.ListIssuerSSOIdentities(ctx, issuer, gocql.UUID{}, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first page %+v, %v", first, err)
	}
	rest, err := r.ListIssuerSSOIdentities(ctx, issuer, first[0].UserID, 10)
	if err != nil || len(rest) != 1 || rest[0].UserID == first[0].UserID {
		t.Fatalf("second page %+v, %v", rest, err)
	}

	if err := r.UnlinkSSOIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetSSOIdentity(ctx, issuer, "s1"); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("an unlinked identity is held: %v", err)
	}
	if list, _ := r.ListUserSSOIdentities(ctx, a); len(list) != 0 {
		t.Fatalf("a still lists %+v", list)
	}
	if all, _ := r.ListIssuerSSOIdentities(ctx, issuer, gocql.UUID{}, 10); len(all) != 1 || all[0].UserID != b {
		t.Fatalf("the issuer still lists %+v", all)
	}
}
