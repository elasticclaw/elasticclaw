package hub

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/elasticclaw/elasticclaw/pkg/secrets"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newSessionTestServer returns a bare Server with a preset JWT signing key,
// bypassing master key loading from the environment.
func newSessionTestServer(key []byte, cfg *types.HubConfig) *Server {
	if cfg == nil {
		cfg = &types.HubConfig{}
	}
	return &Server{hubCfg: cfg, sessionJWTKey: key}
}

func testJWTKey(seed byte) []byte {
	key := bytes.Repeat([]byte{seed}, sessionJWTKeySize)
	return key
}

func TestSessionJWTRoundTrip(t *testing.T) {
	s := newSessionTestServer(testJWTKey(1), nil)

	token, err := s.signSession("octocat", "The Octocat", "https://example.com/a.png")
	if err != nil {
		t.Fatalf("signSession: %v", err)
	}
	if parts := strings.Split(token, "."); len(parts) != 3 {
		t.Fatalf("expected a 3-part JWT, got %d parts", len(parts))
	}

	payload, ok := s.verifySession(token)
	if !ok {
		t.Fatal("verifySession rejected a freshly issued token")
	}
	if payload.Login != "octocat" || payload.Name != "The Octocat" || payload.AvatarURL != "https://example.com/a.png" {
		t.Fatalf("unexpected payload: %+v", payload)
	}

	wantExp := time.Now().Add(githubSessionExpiry).Unix()
	if diff := payload.Exp - wantExp; diff < -5 || diff > 5 {
		t.Fatalf("exp not ~7d from now: got %d, want ~%d", payload.Exp, wantExp)
	}
}

func TestSessionJWTStandardClaims(t *testing.T) {
	s := newSessionTestServer(testJWTKey(1), nil)
	token, err := s.signSession("octocat", "The Octocat", "https://example.com/a.png")
	if err != nil {
		t.Fatalf("signSession: %v", err)
	}

	claims := &sessionClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(tok *jwt.Token) (any, error) {
		return testJWTKey(1), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("token does not parse as a standard HS256 JWT: %v", err)
	}
	if claims.Subject != "octocat" {
		t.Fatalf("sub = %q, want %q", claims.Subject, "octocat")
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil {
		t.Fatal("iat/exp claims missing")
	}
	if claims.Login != "octocat" || claims.Avatar != "https://example.com/a.png" {
		t.Fatalf("custom claims wrong: %+v", claims)
	}
}

func TestSessionJWTExpiredRejected(t *testing.T) {
	key := testJWTKey(1)
	s := newSessionTestServer(key, nil)

	claims := sessionClaims{
		Login: "octocat",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "octocat",
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-8 * 24 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.verifySession(token); ok {
		t.Fatal("expired JWT was accepted")
	}
}

func TestSessionJWTWrongKeyRejected(t *testing.T) {
	signer := newSessionTestServer(testJWTKey(1), nil)
	verifier := newSessionTestServer(testJWTKey(2), nil)

	token, err := signer.signSession("octocat", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := verifier.verifySession(token); ok {
		t.Fatal("JWT signed with a different key was accepted")
	}
}

func TestSessionJWTTamperedRejected(t *testing.T) {
	s := newSessionTestServer(testJWTKey(1), nil)
	token, err := s.signSession("octocat", "", "")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	forged := jwt.MapClaims{"sub": "attacker", "login": "attacker", "exp": time.Now().Add(time.Hour).Unix()}
	body, err := jwt.NewWithClaims(jwt.SigningMethodHS256, forged).SignedString(testJWTKey(9))
	if err != nil {
		t.Fatal(err)
	}
	tampered := parts[0] + "." + strings.Split(body, ".")[1] + "." + parts[2]
	if _, ok := s.verifySession(tampered); ok {
		t.Fatal("tampered JWT was accepted")
	}
}

func TestSessionJWTAlgNoneRejected(t *testing.T) {
	s := newSessionTestServer(testJWTKey(1), nil)
	claims := jwt.MapClaims{"sub": "octocat", "login": "octocat", "exp": time.Now().Add(time.Hour).Unix()}
	token, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.verifySession(token); ok {
		t.Fatal("alg=none JWT was accepted")
	}
}

// TestSessionLegacyTokenStillVerifies covers the upgrade transition window:
// tokens issued by the old hand-rolled HMAC format must remain valid even
// when a JWT signing key is present (dual verification).
func TestSessionLegacyTokenStillVerifies(t *testing.T) {
	cfg := &types.HubConfig{
		Token: "hub-token",
		Auth:  &types.AuthConfig{SessionSecret: "legacy-secret"},
	}
	s := newSessionTestServer(testJWTKey(1), cfg)

	legacy, err := signGitHubSession("legacy-secret", "octocat", "The Octocat", "https://example.com/a.png")
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := s.verifySession(legacy)
	if !ok {
		t.Fatal("legacy session token was rejected after the JWT upgrade")
	}
	if payload.Login != "octocat" || payload.Name != "The Octocat" {
		t.Fatalf("unexpected legacy payload: %+v", payload)
	}

	if _, ok := s.verifySession("garbage.token"); ok {
		t.Fatal("garbage token was accepted")
	}
}

func TestSessionKeyDerivedFromMasterKeyViaHKDF(t *testing.T) {
	masterKey := bytes.Repeat([]byte{7}, secrets.MasterKeySize)
	encoded := base64.StdEncoding.EncodeToString(masterKey)
	t.Setenv(secrets.MasterKeyEnvVar, encoded)

	s := &Server{hubCfg: &types.HubConfig{}}
	key := s.sessionSigningKey()
	if key == nil {
		t.Fatal("expected a derived signing key when a master key is set")
	}
	if bytes.Equal(key, masterKey) {
		t.Fatal("signing key must not be the raw master key")
	}

	want, err := deriveSessionJWTKey(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, want) {
		t.Fatal("derived key does not match HKDF derivation")
	}

	// Derivation is deterministic: a second server derives the same key,
	// so sessions survive hub restarts.
	s2 := &Server{hubCfg: &types.HubConfig{}}
	if !bytes.Equal(s2.sessionSigningKey(), key) {
		t.Fatal("derivation is not deterministic across servers")
	}
}

// TestSessionSignFallsBackToLegacyWithoutMasterKey ensures servers without a
// master key (e.g. bare test setups) still issue working legacy tokens.
func TestSessionSignFallsBackToLegacyWithoutMasterKey(t *testing.T) {
	t.Setenv(secrets.MasterKeyEnvVar, "")
	// Point the master.key lookup at an empty temp dir so a developer's real
	// ~/.elasticclaw/master.key does not leak into the test.
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))

	cfg := &types.HubConfig{
		Token: "hub-token",
		Auth:  &types.AuthConfig{SessionSecret: "legacy-secret"},
	}
	s := &Server{hubCfg: cfg}

	token, err := s.signSession("octocat", "", "")
	if err != nil {
		t.Fatalf("signSession without master key: %v", err)
	}
	if parts := strings.Split(token, "."); len(parts) != 2 {
		t.Fatalf("expected a 2-part legacy token, got %d parts", len(parts))
	}
	if _, ok := s.verifySession(token); !ok {
		t.Fatal("legacy fallback token was rejected")
	}

	// With neither a master key nor a session secret, signing must fail
	// instead of signing with an empty key.
	empty := &Server{hubCfg: &types.HubConfig{}}
	if _, err := empty.signSession("octocat", "", ""); err == nil {
		t.Fatal("expected an error when no signing key material exists")
	}
}
