package push

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/web/render"
)

func pkcs8(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// claims checks a JWT's signature with verify and returns its claims.
func claims(t *testing.T, jwt string, verify func(digest, sig []byte) bool) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", jwt)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !verify(digest[:], sig) {
		t.Fatal("the JWT's signature does not verify")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

var notice = &Notice{Title: "Ann", Body: "hi", Thread: "ch", Data: map[string]string{"channel_id": "ch", "team_id": "tm"}}

func TestFCM(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var (
		signIns, sends atomic.Int32
		expire         atomic.Bool
	)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		signIns.Add(1)
		if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type %q", r.FormValue("grant_type"))
		}
		c := claims(t, r.FormValue("assertion"), func(digest, sig []byte) bool {
			return rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest, sig) == nil
		})
		if c["iss"] != "push@proj.iam.gserviceaccount.com" || c["scope"] != fcmScope || c["aud"] != srv.URL+"/token" {
			t.Errorf("claims %v", c)
		}
		_, _ = io.WriteString(w, `{"access_token":"at-`+strconv.Itoa(int(signIns.Load()))+`","expires_in":3600}`)
	})
	mux.HandleFunc("POST /v1/projects/proj/messages:send", func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if expire.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`)
			return
		}
		var body struct {
			Message struct {
				Token        string            `json:"token"`
				Notification map[string]string `json:"notification"`
				Data         map[string]string `json:"data"`
				Android      struct {
					Priority     string            `json:"priority"`
					Notification map[string]string `json:"notification"`
				} `json:"android"`
			} `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m := body.Message
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-") || m.Notification["title"] != "Ann" ||
			m.Data["channel_id"] != "ch" || m.Android.Priority != "HIGH" || m.Android.Notification["channel_id"] != fcmChannel {
			t.Errorf("sent %s %+v", r.Header.Get("Authorization"), m)
		}
		if m.Token == "gone" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found.","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`)
			return
		}
		if m.Token == "busy" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"errorCode":"QUOTA_EXCEEDED"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"name":"projects/proj/messages/1"}`)
	})

	creds, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "proj", "private_key_id": "kid", "private_key": pkcs8(t, key),
		"client_email": "push@proj.iam.gserviceaccount.com", "token_uri": srv.URL + "/token",
	})
	f, err := NewFCM(creds, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.endpoint = srv.URL
	ctx := context.Background()
	send := func(token string) error {
		return f.Send(ctx, &models.PushDevice{Platform: models.PushFCM, Token: token}, notice)
	}

	if err := send("ok"); err != nil {
		t.Fatal(err)
	}
	if err := send("ok"); err != nil || signIns.Load() != 1 {
		t.Fatalf("second push: %v, signed in %d times", err, signIns.Load())
	}
	if err := send("gone"); !errors.Is(err, ErrDeadToken) {
		t.Fatalf("an unregistered token: %v", err)
	}
	if err := send("busy"); err == nil || errors.Is(err, ErrDeadToken) {
		t.Fatalf("a refused push: %v", err)
	}
	expire.Store(true)
	if err := send("ok"); err != nil || signIns.Load() != 2 {
		t.Fatalf("after the access token expired: %v, signed in %d times", err, signIns.Load())
	}

	if _, err := NewFCM([]byte(`{"project_id":"p"}`), nil); err == nil {
		t.Fatal("took credentials without a key")
	}
}

func TestAPNs(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(digest, sig []byte) bool {
		return len(sig) == 64 && ecdsa.Verify(&key.PublicKey, digest, new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	}
	var (
		hosts  = map[string]int{}
		jwts   = map[string]bool{}
		expire atomic.Bool
	)
	handler := func(host string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hosts[host]++
			jwt, _ := strings.CutPrefix(r.Header.Get("Authorization"), "bearer ")
			jwts[jwt] = true
			if c := claims(t, jwt, verify); c["iss"] != "TEAM" {
				t.Errorf("claims %v", c)
			}
			if r.Header.Get("apns-topic") != "io.example.app" || r.Header.Get("apns-push-type") != "alert" || r.Header.Get("apns-priority") != "10" {
				t.Errorf("headers %v", r.Header)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			aps, _ := body["aps"].(map[string]any)
			alert, _ := aps["alert"].(map[string]any)
			if alert["title"] != "Ann" || aps["thread-id"] != "ch" || body["channel_id"] != "ch" || body["team_id"] != "tm" {
				t.Errorf("payload %v", body)
			}
			if expire.CompareAndSwap(true, false) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"reason":"ExpiredProviderToken"}`)
				return
			}
			switch strings.TrimPrefix(r.URL.Path, "/3/device/") {
			case "gone":
				w.WriteHeader(http.StatusGone)
				_, _ = io.WriteString(w, `{"reason":"Unregistered","timestamp":1}`)
			case "bad":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"reason":"BadDeviceToken"}`)
			case "busy":
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"reason":"TooManyRequests"}`)
			}
		}
	}
	prod := httptest.NewServer(handler("production"))
	defer prod.Close()
	sandbox := httptest.NewServer(handler("sandbox"))
	defer sandbox.Close()

	a, err := NewAPNs([]byte(pkcs8(t, key)), "KEY", "TEAM", "io.example.app", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	a.production, a.sandbox = prod.URL, sandbox.URL
	ctx := context.Background()
	send := func(token string, sandbox bool) error {
		return a.Send(ctx, &models.PushDevice{Platform: models.PushAPNs, Token: token, Sandbox: sandbox}, notice)
	}
	if err := send("ok", false); err != nil {
		t.Fatal(err)
	}
	if err := send("ok", true); err != nil || hosts["production"] != 1 || hosts["sandbox"] != 1 || len(jwts) != 1 {
		t.Fatalf("%v: hosts %v, %d tokens", err, hosts, len(jwts))
	}
	for _, token := range []string{"gone", "bad"} {
		if err := send(token, false); !errors.Is(err, ErrDeadToken) {
			t.Fatalf("%s: %v", token, err)
		}
	}
	if err := send("busy", false); err == nil || errors.Is(err, ErrDeadToken) {
		t.Fatalf("a refused push: %v", err)
	}
	expire.Store(true)
	if err := send("ok", false); err != nil || len(jwts) != 2 {
		t.Fatalf("after the token expired: %v, %d tokens", err, len(jwts))
	}

	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := NewAPNs([]byte(pkcs8(t, rsaKey)), "KEY", "TEAM", "io.example.app", nil); err == nil {
		t.Fatal("took an RSA key")
	}
}

// The push says what the web's notice says.
func TestExcerptMatchesTheWeb(t *testing.T) {
	for _, m := range []*pb.Message{
		{Content: "  hello\n\n  there  "},
		{Content: strings.Repeat("é", 200)},
		{Attachments: []*pb.Attachment{{Voice: true}}},
		{Attachments: []*pb.Attachment{{}}},
		{Attachments: []*pb.Attachment{{}, {}, {}}},
		{Content: "caption", Attachments: []*pb.Attachment{{}}},
	} {
		if got, want := excerpt(m), render.Excerpt(m); got != want {
			t.Errorf("%v: push %q, web %q", m, got, want)
		}
	}
}
