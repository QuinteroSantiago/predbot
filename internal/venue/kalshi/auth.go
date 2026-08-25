package kalshi

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// AuthHeaders builds Kalshi's signed request headers from environment secrets
// (design §14 — secrets never in config files):
//
//	KALSHI_API_KEY_ID        the access key id
//	KALSHI_PRIVATE_KEY_PATH  path to the RSA private key PEM
//
// It signs `timestamp + method + path` with RSASSA-PSS/SHA-256, matching
// Kalshi's REST auth. The exact WS handshake path is [VERIFY] against live docs
// before Phase 4; wsPath is the connection path being opened. If credentials
// are absent it returns an error so the dialer refuses rather than connecting
// unauthenticated (both venues require auth even for public channels, §6.1).
func AuthHeaders(method, wsPath string) (http.Header, error) {
	keyID := os.Getenv("KALSHI_API_KEY_ID")
	keyPath := os.Getenv("KALSHI_PRIVATE_KEY_PATH")
	if keyID == "" || keyPath == "" {
		return nil, errors.New("kalshi: KALSHI_API_KEY_ID and KALSHI_PRIVATE_KEY_PATH must be set")
	}
	key, err := loadRSAKey(keyPath)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig, err := signPSS(key, ts+method+wsPath)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("KALSHI-ACCESS-KEY", keyID)
	h.Set("KALSHI-ACCESS-SIGNATURE", sig)
	h.Set("KALSHI-ACCESS-TIMESTAMP", ts)
	return h, nil
}

func loadRSAKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("kalshi: read private key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("kalshi: no PEM block in private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("kalshi: parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("kalshi: private key is not RSA")
	}
	return rk, nil
}

func signPSS(key *rsa.PrivateKey, msg string) (string, error) {
	sum := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, sum[:], &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA256,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}
