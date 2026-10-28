package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ssenerg/gochat/internal/core/models"
)

const (
	apnsProduction = "https://api.push.apple.com"
	apnsSandbox    = "https://api.sandbox.push.apple.com"
	// apnsTokenAge is how long a signed token is used: Apple refuses one
	// older than an hour and one renewed more often than every 20 minutes.
	apnsTokenAge = 40 * time.Minute
	// apnsExpiry is how long APNs keeps a push for a device out of reach.
	apnsExpiry = 24 * time.Hour
)

// APNs sends pushes through Apple's push service, with a token signed by
// the team's key.
type APNs struct {
	client     *http.Client
	production string
	sandbox    string
	key        *ecdsa.PrivateKey
	keyID      string
	teamID     string
	topic      string

	mu     sync.Mutex
	jwt    string
	signed time.Time
}

// NewAPNs reads a .p8 signing key.
func NewAPNs(p8 []byte, keyID, teamID, topic string, client *http.Client) (*APNs, error) {
	block, _ := pem.Decode(p8)
	if block == nil {
		return nil, errors.New("apns: the key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: the key is not a .p8 key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: the key is not an EC key")
	}
	return &APNs{
		client: client, production: apnsProduction, sandbox: apnsSandbox,
		key: key, keyID: keyID, teamID: teamID, topic: topic,
	}, nil
}

func (a *APNs) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && time.Since(a.signed) < apnsTokenAge {
		return a.jwt, nil
	}
	// ES256 takes r and s as they are, 32 bytes each.
	sign := func(digest []byte) ([]byte, error) {
		r, s, err := ecdsa.Sign(rand.Reader, a.key, digest)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 64)
		r.FillBytes(out[:32])
		s.FillBytes(out[32:])
		return out, nil
	}
	jwt, err := signJWT(map[string]string{"alg": "ES256", "kid": a.keyID}, map[string]any{"iss": a.teamID, "iat": unixNow()}, sign)
	if err != nil {
		return "", err
	}
	a.jwt, a.signed = jwt, time.Now()
	return jwt, nil
}

func (a *APNs) forget(jwt string) {
	a.mu.Lock()
	if a.jwt == jwt {
		a.jwt = ""
	}
	a.mu.Unlock()
}

func (a *APNs) Send(ctx context.Context, d *models.PushDevice, n *Notice) error {
	payload := map[string]any{
		"aps": map[string]any{
			"alert":     map[string]string{"title": n.Title, "body": n.Body},
			"sound":     "default",
			"thread-id": n.Thread,
		},
	}
	for k, v := range n.Data {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	host := a.production
	if d.Sandbox {
		host = a.sandbox
	}
	for attempt := 0; ; attempt++ {
		jwt, err := a.token()
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/3/device/"+d.Token, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "bearer "+jwt)
		req.Header.Set("apns-topic", a.topic)
		req.Header.Set("apns-push-type", "alert")
		req.Header.Set("apns-priority", "10")
		req.Header.Set("apns-expiration", strconv.FormatInt(time.Now().Add(apnsExpiry).Unix(), 10))
		resp, err := a.client.Do(req)
		if err != nil {
			return err
		}
		answer := readBody(resp)
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		var out struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(answer, &out)
		switch out.Reason {
		case "BadDeviceToken", "DeviceTokenNotForTopic", "Unregistered":
			return ErrDeadToken
		case "ExpiredProviderToken", "InvalidProviderToken":
			if attempt == 0 {
				a.forget(jwt)
				continue
			}
		}
		if resp.StatusCode == http.StatusGone {
			return ErrDeadToken
		}
		return fmt.Errorf("apns: %d %s", resp.StatusCode, out.Reason)
	}
}
