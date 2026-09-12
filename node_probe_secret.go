package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const nodeProbeSecretCipherPrefix = "v1:"

func nodeProbeSecretKey(secret []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("meridian-node-probe-secret-v1\x00"))
	_, _ = hash.Write(secret)
	return hash.Sum(nil)
}

func encryptNodeProbeSecretWithSecret(value string, secret []byte) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 32 || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("invalid node probe secret")
	}
	if len(secret) < 32 {
		return "", errors.New("JWT secret is unavailable")
	}
	block, err := aes.NewCipher(nodeProbeSecretKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(value), []byte("meridian-node-probe-secret"))
	return nodeProbeSecretCipherPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptNodeProbeSecretWithSecret(ciphertext string, secret []byte) (string, error) {
	if !strings.HasPrefix(ciphertext, nodeProbeSecretCipherPrefix) || len(secret) < 32 {
		return "", errors.New("invalid node probe secret ciphertext")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(ciphertext, nodeProbeSecretCipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decode node probe secret: %w", err)
	}
	block, err := aes.NewCipher(nodeProbeSecretKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(payload) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("invalid node probe secret ciphertext")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], []byte("meridian-node-probe-secret"))
	if err != nil {
		return "", errors.New("invalid node probe secret ciphertext")
	}
	value := strings.TrimSpace(string(plain))
	if len(value) < 32 || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("invalid node probe secret")
	}
	return value, nil
}

func newNodeProbeSecret() (string, error) {
	return newNodeToken()
}

// decodeNodeProbeSecret converts the encrypted-at-rest plaintext representation
// (a base64url token) into the raw 32-byte secret used on the wire. Keeping this
// conversion in one place prevents accidentally base64-encoding the textual
// token a second time when building Agent configs or scheduler probes.
func decodeNodeProbeSecret(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("invalid node probe secret")
	}
	return decoded, nil
}

// dNodeProbeSecret returns the plaintext only at the two places that need it:
// the Agent config response and the Controller's private readiness probe. The
// database stores only JWT-secret-encrypted material; the in-memory ControlNode
// never exposes this value through JSON.
func dNodeProbeSecret(db *DB, node ControlNode) (string, error) {
	if db == nil || db.db == nil || node.ID <= 0 {
		return "", errors.New("node probe secret is unavailable")
	}
	if strings.TrimSpace(node.probeSecretCiphertext) != "" {
		secret, err := decryptNodeProbeSecretWithSecret(node.probeSecretCiphertext, jwtSecret)
		if err == nil {
			return secret, nil
		}
		if jwtSecretEphemeral {
			return "", err
		}
		return rotateNodeProbeSecret(db, node)
	}
	if jwtSecretEphemeral {
		return "", errPersistentJWTRequired
	}
	secret, err := newNodeProbeSecret()
	if err != nil {
		return "", err
	}
	ciphertext, err := encryptNodeProbeSecretWithSecret(secret, jwtSecret)
	if err != nil {
		return "", err
	}
	result, err := db.db.Exec("UPDATE control_nodes SET probe_secret_ciphertext=?,updated_at_ms=? WHERE id=? AND probe_secret_ciphertext=''", ciphertext, time.Now().UnixMilli(), node.ID)
	if err != nil {
		return "", err
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return "", rowsErr
	} else if affected == 0 {
		var existing string
		if err := db.db.QueryRow("SELECT probe_secret_ciphertext FROM control_nodes WHERE id=?", node.ID).Scan(&existing); err != nil {
			return "", err
		}
		return decryptNodeProbeSecretWithSecret(existing, jwtSecret)
	}
	return secret, nil
}

// rotateNodeProbeSecret repairs a ciphertext written with an old signing key
// without touching the node identity, enrollment token, or traffic counters.
// The conditional update makes concurrent config requests converge on one
// secret while preserving a successfully rotated value.
func rotateNodeProbeSecret(db *DB, node ControlNode) (string, error) {
	if db == nil || db.db == nil || node.ID <= 0 {
		return "", errors.New("node probe secret is unavailable")
	}
	if jwtSecretEphemeral || len(jwtSecret) < 32 {
		return "", errors.New("persistent JWT_SECRET is required to rotate node probe secret")
	}
	secret, err := newNodeProbeSecret()
	if err != nil {
		return "", err
	}
	ciphertext, err := encryptNodeProbeSecretWithSecret(secret, jwtSecret)
	if err != nil {
		return "", err
	}
	// Rotating the probe secret changes the runtime config payload, so the
	// node's config revision must advance: without it, the scheduler probes
	// with the new secret while the agent is still serving the old config
	// until its next unconditional 60s poll.
	result, err := db.db.Exec(`UPDATE control_nodes SET probe_secret_ciphertext=?,
		config_revision=config_revision+1, desired_config_hash='', desired_config_revision=0, config_dirty=1, updated_at_ms=?
		WHERE id=? AND probe_secret_ciphertext=?`, ciphertext, time.Now().UnixMilli(), node.ID, node.probeSecretCiphertext)
	if err != nil {
		return "", err
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return "", rowsErr
	} else if affected == 1 {
		return secret, nil
	}
	var existing string
	if err := db.db.QueryRow("SELECT probe_secret_ciphertext FROM control_nodes WHERE id=?", node.ID).Scan(&existing); err != nil {
		return "", err
	}
	return decryptNodeProbeSecretWithSecret(existing, jwtSecret)
}
