package scylla

import (
	"context"
	"testing"
	"time"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

func TestPushDevicesLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	user, now := ids.New(), time.Now().UTC().Truncate(time.Millisecond)
	token := "fcm:" + ids.New().String()
	d := &models.PushDevice{UserID: user, SessionHash: secure.Hash(secure.Token()), Platform: models.PushFCM, Token: token, CreatedAt: now}
	if err := r.PutPushDevice(ctx, d, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetPushDevice(ctx, user, d.SessionHash)
	if err != nil || got.Token != token || got.Platform != models.PushFCM || got.Sandbox || !got.CreatedAt.Equal(now) {
		t.Fatalf("device %+v, %v", got, err)
	}
	other := &models.PushDevice{UserID: user, SessionHash: secure.Hash(secure.Token()), Platform: models.PushAPNs, Token: "ab" + ids.New().String(), Sandbox: true, CreatedAt: now}
	if err := r.PutPushDevice(ctx, other, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if list, err := r.ListPushDevices(ctx, user); err != nil || len(list) != 2 {
		t.Fatalf("devices %d, %v", len(list), err)
	}
	if id, hash, err := r.PushTokenHolder(ctx, models.PushFCM, token); err != nil || id != user || string(hash) != string(d.SessionHash) {
		t.Fatalf("holder %v %x, %v", id, hash, err)
	}
	if id, hash, err := r.PushTokenHolder(ctx, models.PushAPNs, token); err != nil || !ids.IsZero(id) || hash != nil {
		t.Fatalf("a token of another platform is held by %v %x, %v", id, hash, err)
	}

	if err := r.DeletePushDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetPushDevice(ctx, user, d.SessionHash); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a deleted device: %v", err)
	}
	if id, _, err := r.PushTokenHolder(ctx, models.PushFCM, token); err != nil || !ids.IsZero(id) {
		t.Fatalf("a deleted device's token is held by %v, %v", id, err)
	}
	if err := r.DeletePushToken(ctx, other.Platform, other.Token); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := r.PushTokenHolder(ctx, other.Platform, other.Token); !ids.IsZero(id) {
		t.Fatal("a forgotten token is still held")
	}
	if list, _ := r.ListPushDevices(ctx, user); len(list) != 1 || !list[0].Sandbox {
		t.Fatalf("devices left %+v", list)
	}

	// A dead token takes its session's device only while the device holds
	// it.
	newer := *other
	newer.Token = "cd" + ids.New().String()
	if err := r.PutPushDevice(ctx, &newer, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.ForgetPushToken(ctx, other); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetPushDevice(ctx, user, other.SessionHash); err != nil || got.Token != newer.Token {
		t.Fatalf("after its old token died: %+v, %v", got, err)
	}
	if err := r.ForgetPushToken(ctx, &newer); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetPushDevice(ctx, user, other.SessionHash); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("after its token died: %v", err)
	}
	if id, _, _ := r.PushTokenHolder(ctx, newer.Platform, newer.Token); !ids.IsZero(id) {
		t.Fatal("a dead token is still held")
	}
}
