package push

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ssenerg/gochat/internal/core/models"
)

// Notice is what a push shows, and the data the app opens it with.
type Notice struct {
	Title, Body string
	// Thread groups a channel's pushes together.
	Thread string
	Data   map[string]string
}

// Provider delivers pushes to the devices of one platform.
type Provider interface {
	Send(ctx context.Context, d *models.PushDevice, n *Notice) error
}

// ErrDeadToken says the provider no longer takes the device's token.
var ErrDeadToken = errors.New("push: the device's token is no longer valid")

// maxResponse bounds what is read of a provider's answer.
const maxResponse = 64 << 10

// expiry is how long FCM and APNs keep a push for a device out of reach.
const expiry = 24 * time.Hour

// Client is the HTTP client of the providers. APNs needs HTTP/2, which
// Go's transport speaks over TLS.
func Client() *http.Client {
	return &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
}

// Providers builds the providers cfg turns on, by platform.
func Providers(cfg *Config, client *http.Client) (map[string]Provider, error) {
	out := map[string]Provider{}
	if cfg.FCM.Enabled() {
		creds, err := secret(cfg.FCM.Credentials, cfg.FCM.CredentialsFile)
		if err != nil {
			return nil, err
		}
		fcm, err := NewFCM(creds, client)
		if err != nil {
			return nil, err
		}
		out[models.PushFCM] = fcm
	}
	if cfg.APNs.Enabled() {
		key, err := secret(cfg.APNs.Key, cfg.APNs.KeyFile)
		if err != nil {
			return nil, err
		}
		apns, err := NewAPNs(key, cfg.APNs.KeyID, cfg.APNs.TeamID, cfg.APNs.Topic, client)
		if err != nil {
			return nil, err
		}
		out[models.PushAPNs] = apns
	}
	return out, nil
}

// signJWT returns a JSON Web Token of header and claims, signed by sign
// over the SHA-256 of its first two parts.
func signJWT(header, claims any, sign func(digest []byte) ([]byte, error)) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	input := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	digest := sha256.Sum256([]byte(input))
	sig, err := sign(digest[:])
	if err != nil {
		return "", err
	}
	return input + "." + enc.EncodeToString(sig), nil
}

// signer signs digests with key, as crypto.Signer does.
func signer(key crypto.Signer) func([]byte) ([]byte, error) {
	return func(digest []byte) ([]byte, error) { return key.Sign(rand.Reader, digest, crypto.SHA256) }
}

func readBody(resp *http.Response) []byte {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	_ = resp.Body.Close()
	return b
}

func unixNow() int64 { return time.Now().Unix() }
