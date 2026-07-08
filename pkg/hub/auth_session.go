package hub

// Web session tokens as standard JWTs.
//
// Sessions are HS256 JWTs signed with a key derived from the master key
// (pkg/secrets) via HKDF-SHA256 — no longer the raw hub token. The legacy
// hand-rolled HMAC format (signGitHubSession/verifyGitHubSession in
// auth_github.go) is still *accepted* for one release so existing sessions
// survive the upgrade; issuance uses JWTs whenever a master key is available.
// TODO(next release): drop the legacy verification fallback.

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"log"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/elasticclaw/elasticclaw/pkg/secrets"
)

// sessionJWTInfo is the HKDF info string that binds the derived key to this
// specific purpose (domain separation from other keys derived from the master key).
const sessionJWTInfo = "elasticclaw/hub session-jwt v1"

// sessionJWTKeySize is the size in bytes of the derived HS256 signing key.
const sessionJWTKeySize = 32

// sessionClaims are the JWT claims carried by web session tokens.
type sessionClaims struct {
	Login  string `json:"login"`
	Name   string `json:"name,omitempty"`
	Avatar string `json:"avatar,omitempty"`
	jwt.RegisteredClaims
}

// deriveSessionJWTKey derives the HS256 session signing key from the master
// key via HKDF-SHA256.
func deriveSessionJWTKey(masterKey []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, masterKey, nil, sessionJWTInfo, sessionJWTKeySize)
}

// sessionSigningKey returns the HKDF-derived JWT signing key, deriving it from
// the master key on first use. It returns nil when no master key is available;
// callers then fall back to the legacy HMAC session format.
func (s *Server) sessionSigningKey() []byte {
	s.sessionJWTOnce.Do(func() {
		if s.sessionJWTKey != nil {
			return // preset (tests)
		}
		masterKey, err := secrets.LoadMasterKey()
		if err != nil {
			log.Printf("[session] master key unavailable, using legacy session signing: %v", err)
			return
		}
		key, err := deriveSessionJWTKey(masterKey)
		if err != nil {
			log.Printf("[session] failed to derive session JWT key: %v", err)
			return
		}
		s.sessionJWTKey = key
	})
	return s.sessionJWTKey
}

// signSession issues a session token for an authenticated GitHub user.
// It produces a standard HS256 JWT when the master-key-derived signing key is
// available, and falls back to the legacy HMAC format otherwise.
func (s *Server) signSession(login, name, avatarURL string) (string, error) {
	key := s.sessionSigningKey()
	if key == nil {
		secret := s.webSessionSecret()
		if secret == "" {
			return "", errors.New("no session signing key available")
		}
		return signGitHubSession(secret, login, name, avatarURL)
	}
	now := time.Now()
	claims := sessionClaims{
		Login:  login,
		Name:   name,
		Avatar: avatarURL,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   login,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(githubSessionExpiry)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
}

// verifySession validates a web session token and returns its payload.
// It first tries the standard JWT format, then falls back to the legacy
// hand-rolled HMAC format (dual verification, kept for one release so
// existing sessions are not logged out on upgrade).
func (s *Server) verifySession(token string) (*githubSessionPayload, bool) {
	if key := s.sessionSigningKey(); key != nil {
		claims := &sessionClaims{}
		parsed, err := jwt.ParseWithClaims(token, claims,
			func(t *jwt.Token) (any, error) { return key, nil },
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithExpirationRequired(),
		)
		if err == nil && parsed.Valid {
			login := claims.Login
			if login == "" {
				login = claims.Subject
			}
			if login == "" {
				return nil, false
			}
			var exp int64
			if claims.ExpiresAt != nil {
				exp = claims.ExpiresAt.Unix()
			}
			return &githubSessionPayload{
				Login:     login,
				Name:      claims.Name,
				AvatarURL: claims.Avatar,
				Exp:       exp,
			}, true
		}
	}
	// Legacy format fallback (remove after one release).
	if secret := s.webSessionSecret(); secret != "" {
		return verifyGitHubSession(secret, token)
	}
	return nil, false
}
