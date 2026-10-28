package push

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ssenerg/gochat/internal/core/models"
)

const (
	fcmEndpoint = "https://fcm.googleapis.com"
	fcmScope    = "https://www.googleapis.com/auth/firebase.messaging"
	googleToken = "https://oauth2.googleapis.com/token"
	// fcmChannel is the Android notification channel the app makes for
	// messages.
	fcmChannel = "messages"
)

// FCM sends pushes through Firebase Cloud Messaging's HTTP v1 API, signed
// in as a service account.
type FCM struct {
	client   *http.Client
	endpoint string
	project  string
	email    string
	keyID    string
	tokenURI string
	key      *rsa.PrivateKey

	mu      sync.Mutex
	access  string
	expires time.Time
}

// NewFCM reads a service account's JSON key.
func NewFCM(credentials []byte, client *http.Client) (*FCM, error) {
	var sa struct {
		ProjectID    string `json:"project_id"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
		ClientEmail  string `json:"client_email"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(credentials, &sa); err != nil {
		return nil, fmt.Errorf("fcm: the credentials are not a service account's JSON key: %w", err)
	}
	if sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("fcm: the credentials lack project_id, client_email or private_key")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, errors.New("fcm: the private key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, _ = k.(*rsa.PrivateKey)
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	}
	if key == nil {
		return nil, errors.New("fcm: the private key is not an RSA key")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = googleToken
	}
	return &FCM{
		client: client, endpoint: fcmEndpoint, project: sa.ProjectID, email: sa.ClientEmail,
		keyID: sa.PrivateKeyID, tokenURI: sa.TokenURI, key: key,
	}, nil
}

// token returns an access token, signing in again a minute before the
// last one expires.
func (f *FCM) token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access != "" && time.Now().Before(f.expires) {
		return f.access, nil
	}
	now := unixNow()
	assertion, err := signJWT(
		map[string]string{"alg": "RS256", "typ": "JWT", "kid": f.keyID},
		map[string]any{"iss": f.email, "scope": fcmScope, "aud": f.tokenURI, "iat": now, "exp": now + 3600},
		signer(f.key),
	)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	body := readBody(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fcm: signing in failed with %d: %s", resp.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", errors.New("fcm: signing in gave no access token")
	}
	f.access = out.AccessToken
	f.expires = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - time.Minute)
	return f.access, nil
}

func (f *FCM) forget(access string) {
	f.mu.Lock()
	if f.access == access {
		f.access = ""
	}
	f.mu.Unlock()
}

func (f *FCM) Send(ctx context.Context, d *models.PushDevice, n *Notice) error {
	msg := map[string]any{
		"token":        d.Token,
		"notification": map[string]string{"title": n.Title, "body": n.Body},
		"data":         n.Data,
		"android": map[string]any{
			"priority":     "HIGH",
			"notification": map[string]string{"channel_id": fcmChannel},
		},
	}
	body, err := json.Marshal(map[string]any{"message": msg})
	if err != nil {
		return err
	}
	// A token rejected as expired is renewed once.
	for attempt := 0; ; attempt++ {
		access, err := f.token(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.endpoint+"/v1/projects/"+url.PathEscape(f.project)+"/messages:send", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+access)
		req.Header.Set("Content-Type", "application/json")
		resp, err := f.client.Do(req)
		if err != nil {
			return err
		}
		answer := readBody(resp)
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			f.forget(access)
			continue
		}
		return fcmError(resp.StatusCode, answer)
	}
}

// fcmError reads FCM's error, telling a token it no longer takes apart.
func fcmError(status int, body []byte) error {
	var out struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	code := out.Error.Status
	for _, d := range out.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
		}
	}
	switch {
	case code == "UNREGISTERED", code == "SENDER_ID_MISMATCH",
		code == "INVALID_ARGUMENT" && strings.Contains(out.Error.Message, "registration token"):
		return ErrDeadToken
	}
	return fmt.Errorf("fcm: %d %s: %s", status, code, out.Error.Message)
}
