// Package secure holds the primitives every credential goes through.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const tokenBytes = 32

// Token returns 256 random bits, base64url encoded.
func Token() string {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// botPrefix starts a bot's token, telling it from a session's.
const botPrefix = "b_"

// BotToken returns a bot's token: a Token behind the bot prefix.
func BotToken() string { return botPrefix + Token() }

// IsBotToken reports whether s is shaped like a BotToken.
func IsBotToken(s string) bool {
	rest, ok := strings.CutPrefix(s, botPrefix)
	return ok && ValidToken(rest)
}

// ValidCredential reports whether s is shaped like a session's or a bot's
// token.
func ValidCredential(s string) bool { return ValidToken(s) || IsBotToken(s) }

// ValidToken reports whether s is shaped like a Token, so a lookup is never
// spent on garbage.
func ValidToken(s string) bool {
	if len(s) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}

// MaskToken hides token behind a fresh random pad, so pages carrying it
// differ every time and compressing them gives nothing of it away.
func MaskToken(token string) string {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenBytes {
		return token
	}
	out := make([]byte, 2*tokenBytes)
	if _, err := rand.Read(out[:tokenBytes]); err != nil {
		panic(err)
	}
	pad, hidden := out[:tokenBytes], out[tokenBytes:]
	for i, b := range raw {
		hidden[i] = b ^ pad[i]
	}
	return base64.RawURLEncoding.EncodeToString(out)
}

// UnmaskToken returns the token MaskToken hid in masked.
func UnmaskToken(masked string) (string, bool) {
	if len(masked) != base64.RawURLEncoding.EncodedLen(2*tokenBytes) {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(masked)
	if err != nil {
		return "", false
	}
	raw := make([]byte, tokenBytes)
	for i := range raw {
		raw[i] = b[tokenBytes+i] ^ b[i]
	}
	return base64.RawURLEncoding.EncodeToString(raw), true
}

// Hash is the stored form of a token: tokens carry 256 bits of entropy, so a
// fast hash is enough.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// PublicID derives a non-secret identifier from a token hash, safe to log and
// to show.
func PublicID(hash []byte) string {
	if len(hash) < 12 {
		return hex.EncodeToString(hash)
	}
	return hex.EncodeToString(hash[:12])
}

func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// DeriveKey derives a 32-byte key for purpose from key, so one secret can
// key several unrelated uses.
func DeriveKey(key []byte, purpose string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("gochat/"))
	m.Write([]byte(purpose))
	return m.Sum(nil)
}

func MAC(key []byte, parts ...string) string {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Sealer encrypts small secrets at rest with AES-256-GCM.
type Sealer struct {
	aead cipher.AEAD
}

func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("sealing key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal binds the ciphertext to aad, so a sealed value cannot be moved to
// another row.
func (s *Sealer) Seal(plaintext, aad []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plaintext)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad)
}

func (s *Sealer) Open(sealed, aad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, errors.New("sealed value is too short")
	}
	out, err := s.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, fmt.Errorf("failed to open sealed value: %w", err)
	}
	return out, nil
}

// DecodeKey reads a 32-byte key given in base64 (standard or url).
func DecodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != 32 {
				return nil, errors.New("key must decode to 32 bytes")
			}
			return b, nil
		}
	}
	return nil, errors.New("key must be base64")
}
