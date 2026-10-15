package polymarketus

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// Verify the full auth path: a base64 seed in the env -> signed headers whose
// signature validates against the corresponding public key.
func TestAuthHeadersEd25519(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seed := priv.Seed() // 32-byte seed
	t.Setenv("POLYMARKET_US_KEY_ID", "test-key-id")
	t.Setenv("POLYMARKET_US_SECRET", base64.StdEncoding.EncodeToString(seed))

	h, err := AuthHeaders("GET", "/v1/ws/markets")
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("X-PM-Access-Key") != "test-key-id" {
		t.Errorf("access-key header = %q", h.Get("X-PM-Access-Key"))
	}
	ts := h.Get("X-PM-Timestamp")
	if ts == "" {
		t.Fatal("missing timestamp header")
	}
	sig, err := base64.StdEncoding.DecodeString(h.Get("X-PM-Signature"))
	if err != nil {
		t.Fatalf("signature not base64: %v", err)
	}
	if !ed25519.Verify(pub, signedMessage(ts, "GET", "/v1/ws/markets"), sig) {
		t.Error("signature did not verify against the public key")
	}
}

func TestAuthHeadersRejectsMissingCreds(t *testing.T) {
	t.Setenv("POLYMARKET_US_KEY_ID", "")
	t.Setenv("POLYMARKET_US_SECRET", "")
	if _, err := AuthHeaders("GET", "/v1/ws/markets"); err == nil {
		t.Error("expected error when creds are absent")
	}
}

// A hex-encoded 64-byte full key must also load.
func TestAuthHeadersHexFullKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv("POLYMARKET_US_KEY_ID", "k")
	t.Setenv("POLYMARKET_US_SECRET", hexEncode(priv))
	if _, err := AuthHeaders("GET", "/p"); err != nil {
		t.Errorf("hex full key should load: %v", err)
	}
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0xf]
	}
	return string(out)
}
