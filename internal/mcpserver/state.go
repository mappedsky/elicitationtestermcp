package mcpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// state is what the server carries between the input-required result and the
// retry that answers it. SEP-2322 requires an unauthenticated server to sign
// and verify this value, and there is nowhere else to keep it: a client may
// answer on a different connection, against a different replica, an hour
// later. Everything the retry needs is therefore in here rather than in
// session memory.
type state struct {
	Scenario string `json:"scenario"`
	Tool     string `json:"tool"`
	Round    int    `json:"round"`
	// ArgsDigest pins the state to the arguments it was issued for, so a
	// retry cannot reuse one scenario's state to answer another.
	ArgsDigest string `json:"args"`
	IssuedUnix int64  `json:"issued"`
	Nonce      string `json:"nonce"`
}

// signer mints and verifies request states.
type signer struct {
	key []byte
	ttl time.Duration
}

// newSigner derives the signing key from secret. An empty secret produces a
// random per-process key, which invalidates every parked elicitation on
// restart; pass a fixed secret when a client parks requests for longer than
// the server's own lifetime.
func newSigner(secret string, ttl time.Duration) (*signer, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("state TTL must be positive")
	}
	key := make([]byte, 32)
	if secret == "" {
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate state key: %w", err)
		}
	} else {
		sum := sha256.Sum256([]byte(secret))
		key = sum[:]
	}
	return &signer{key: key, ttl: ttl}, nil
}

func (s *signer) sign(st state) (string, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	st.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	st.IssuedUnix = time.Now().Unix()
	payload, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("marshal state: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.mac(body)), nil
}

// verify returns the state carried by token, rejecting anything this server
// did not sign, did not issue for these arguments, or issued too long ago.
func (s *signer) verify(token, tool, argsDigest string) (state, error) {
	body, signature, found := strings.Cut(token, ".")
	if !found {
		return state{}, fmt.Errorf("malformed request state")
	}
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(provided, s.mac(body)) {
		return state{}, fmt.Errorf("request state signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return state{}, fmt.Errorf("malformed request state")
	}
	var st state
	if err := json.Unmarshal(payload, &st); err != nil {
		return state{}, fmt.Errorf("malformed request state")
	}
	if time.Since(time.Unix(st.IssuedUnix, 0)) > s.ttl {
		return state{}, fmt.Errorf("request state expired after %s", s.ttl)
	}
	if st.Tool != tool {
		return state{}, fmt.Errorf("request state was issued for tool %q", st.Tool)
	}
	if st.ArgsDigest != argsDigest {
		return state{}, fmt.Errorf("request state was issued for different arguments")
	}
	return st, nil
}

func (s *signer) mac(body string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

// digest is a short, stable fingerprint. It identifies a value in the
// exchange log without reprinting it, which keeps a submitted secret out of
// the default read-back.
func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return fmt.Sprintf("%x", sum[:6])
}
