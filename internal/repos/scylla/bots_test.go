package scylla

import (
	"context"
	"testing"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
)

func TestBotsLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	owner, now := ids.New(), time.Now().UTC().Truncate(time.Millisecond)
	name := "live" + ids.New().String()[:8] + "_bot"
	u := &models.User{ID: ids.New(), Username: name, DisplayName: "Live", Role: models.SystemRoleUser, CreatedAt: now, UpdatedAt: now, Bot: true, BotOwner: owner}
	hash := secure.Hash(secure.BotToken())
	if err := r.CreateBot(ctx, u, &models.Bot{ID: u.ID, OwnerID: owner, Description: "d", TokenHash: hash, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBot(ctx, &models.User{ID: ids.New(), Username: name, BotOwner: owner}, &models.Bot{ID: ids.New(), TokenHash: hash}); err == nil {
		t.Fatal("a second bot took the username")
	}
	got, err := r.GetUser(ctx, u.ID)
	if err != nil || !got.Bot || got.BotOwner != owner || got.Email != "" {
		t.Fatalf("the bot's user %+v, %v", got, err)
	}
	if id, err := r.BotOfToken(ctx, hash); err != nil || id != u.ID {
		t.Fatalf("token's bot %v, %v", id, err)
	}
	newHash := secure.Hash(secure.BotToken())
	if err := r.ReplaceBotToken(ctx, u.ID, hash, newHash); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BotOfToken(ctx, hash); err == nil {
		t.Fatal("the old token still names the bot")
	}
	if err := r.SetWebhook(ctx, u.ID, "https://example.com/h", "s"); err != nil {
		t.Fatal(err)
	}
	hooks, err := r.ListWebhooks(ctx)
	found := false
	for _, b := range hooks {
		found = found || (b.ID == u.ID && b.WebhookURL == "https://example.com/h")
	}
	if err != nil || !found {
		t.Fatalf("webhooks %v, %v", hooks, err)
	}
	b, err := r.GetBot(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteBot(ctx, b); err != nil {
		t.Fatal(err)
	}
	if listed, _ := r.ListBotIDs(ctx, owner); len(listed) != 0 {
		t.Fatalf("a deleted bot is listed: %v", listed)
	}
	if got, _ := r.GetUser(ctx, u.ID); !got.Deactivated {
		t.Fatal("a deleted bot's user stayed active")
	}

	// A bot's membership, and a bot's buttons.
	ch := ids.New()
	if err := r.AddChannelMember(ctx, &models.ChannelMember{ChannelID: ch, UserID: u.ID, TeamID: ids.New(), Role: models.ChannelRoleMember, JoinedAt: now, Bot: true, BotSeesAll: true}); err != nil {
		t.Fatal(err)
	}
	members, err := r.ListChannelMembers(ctx, ch)
	if err != nil || len(members) != 1 || !members[0].Bot || !members[0].BotSeesAll {
		t.Fatalf("members %+v, %v", members, err)
	}
	m := &models.Message{ChannelID: ch, ID: ids.NewTime(), UserID: u.ID, Content: "pick", Markup: []byte{1, 2}}
	m.Bucket = ids.BucketOf(m.ID)
	if err := r.InsertMessage(ctx, m, true); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetMessage(ctx, ch, m.ID); err != nil || string(got.Markup) != string([]byte{1, 2}) {
		t.Fatalf("message %+v, %v", got, err)
	}
	if err := r.SetMarkup(ctx, ch, gocql.UUID{}, m.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetMessage(ctx, ch, m.ID); len(got.Markup) != 0 {
		t.Fatal("the buttons stayed")
	}
}
