package polymarketus

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// AuthHeaders builds Polymarket US's Ed25519-signed request headers from the
// environment (design §14):
//
//	POLYMARKET_US_KEY_ID   the access key id
//	POLYMARKET_US_SECRET   the Ed25519 secret (32-byte seed or 64-byte key), base64 or hex
//
// Per the API docs, requests are signed with the secret key using Ed25519, with:
//
//	X-PM-Access-Key: {keyId}
//	X-PM-Timestamp:  {unix_ms}
//	X-PM-Signature:  {base64(signature)}
//
// [VERIFY] the exact signed message. The docs give the headers but not the
// payload construction; this signs `{timestamp}{method}{path}` (the common
// convention). If the venue signs a different string, only signedMessage below
// changes. Returns an error when credentials are absent so the dialer refuses to
// connect unauthenticated.
func AuthHeaders(method, path string) (http.Header, error) {
	keyID := os.Getenv("POLYMARKET_US_KEY_ID")
	secret := os.Getenv("POLYMARKET_US_SECRET")
	if keyID == "" || secret == "" {
		return nil, errors.New("polymarketus: POLYMARKET_US_KEY_ID and POLYMARKET_US_SECRET must be set")
	}
	priv, err := loadEd25519Key(secret)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig := ed25519.Sign(priv, signedMessage(ts, method, path))

	h := http.Header{}
	h.Set("X-PM-Access-Key", keyID)
	h.Set("X-PM-Timestamp", ts)
	h.Set("X-PM-Signature", base64.StdEncoding.EncodeToString(sig))
	return h, nil
}

// signedMessage is the payload signed with the Ed25519 secret. [VERIFY] against
// live docs — this assumes timestamp + method + path.
func signedMessage(ts, method, path string) []byte {
	return []byte(ts + method + path)
}

// loadEd25519Key decodes the secret (base64 or hex) into an Ed25519 private key.
// Accepts a 32-byte seed (expanded via NewKeyFromSeed) or a 64-byte full key.
func loadEd25519Key(secret string) (ed25519.PrivateKey, error) {
	raw, err := decodeKey(secret)
	if err != nil {
		return nil, err
	}
	switch len(raw) {
	case ed25519.SeedSize: // 32
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize: // 64
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("polymarketus: secret is %d bytes, want 32 (seed) or 64 (key)", len(raw))
	}
}

func decodeKey(s string) ([]byte, error) {
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := dec(s); err == nil && (len(b) == ed25519.SeedSize || len(b) == ed25519.PrivateKeySize) {
			return b, nil
		}
	}
	return nil, errors.New("polymarketus: could not decode POLYMARKET_US_SECRET as base64 or hex (expect 32- or 64-byte key)")
}
