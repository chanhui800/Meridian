package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

var jwtSecret []byte
var jwtSecretEphemeral bool

const (
	sessionCookieName = "meridian_session"
	sessionDuration   = 72 * time.Hour
)

func init() {
	var err error
	jwtSecret, jwtSecretEphemeral, err = resolveJWTSecret(os.Getenv("JWT_SECRET"))
	if err != nil {
		panic(err)
	}
}

func resolveJWTSecret(value string) ([]byte, bool, error) {
	if value != "" {
		if len(value) < 32 {
			return nil, false, fmt.Errorf("JWT_SECRET must be at least 32 bytes")
		}
		return []byte(value), false, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, false, fmt.Errorf("generate JWT secret: %w", err)
	}
	return secret, true, nil
}

type sessionTokenClaims struct {
	Sub     int64  `json:"sub"`
	Name    string `json:"name"`
	Version int64  `json:"ver,omitempty"`
	Exp     int64  `json:"exp"`
}

func generateToken(userID int64, username string) (string, error) {
	return generateTokenWithVersion(userID, username, 1)
}

func generateTokenWithVersion(userID int64, username string, version int64) (string, error) {
	if version < 1 {
		version = 1
	}
	header := jwtHeaderEncoded
	payload, err := json.Marshal(sessionTokenClaims{
		Sub:     userID,
		Name:    username,
		Version: version,
		Exp:     time.Now().Add(sessionDuration).Unix(),
	})
	if err != nil {
		return "", err
	}
	payloadEnc := base64url(payload)
	sig := hmacSHA256(header+"."+payloadEnc, jwtSecret)
	return header + "." + payloadEnc + "." + sig, nil
}

func validateToken(token string) (int64, string, error) {
	claims, err := validateTokenClaims(token)
	if err != nil {
		return 0, "", err
	}
	return claims.Sub, claims.Name, nil
}

func validateTokenClaims(token string) (sessionTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return sessionTokenClaims{}, fmt.Errorf("invalid token")
	}
	if parts[0] != jwtHeaderEncoded {
		return sessionTokenClaims{}, fmt.Errorf("invalid token header")
	}
	expectedSig := hmacSHA256(parts[0]+"."+parts[1], jwtSecret)
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return sessionTokenClaims{}, fmt.Errorf("invalid signature")
	}
	payload, err := base64urlDecode(parts[1])
	if err != nil {
		return sessionTokenClaims{}, err
	}
	var claims sessionTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return sessionTokenClaims{}, err
	}
	if time.Now().Unix() > claims.Exp {
		return sessionTokenClaims{}, fmt.Errorf("token expired")
	}
	// Tokens created before session versions were introduced did not carry a
	// ver claim. Treat them as generation 1 so upgrades do not log out every
	// administrator, while the next account change still revokes them.
	if claims.Version < 1 {
		claims.Version = 1
	}
	return claims, nil
}

var jwtHeaderEncoded = base64url([]byte(`{"alg":"HS256","typ":"JWT"}`))

func hmacSHA256(data string, key []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return base64url(h.Sum(nil))
}

func base64url(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func base64urlDecode(s string) ([]byte, error) {
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}
