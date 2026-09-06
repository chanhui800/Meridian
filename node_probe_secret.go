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

// dNodeProbeSecret returns the plaintext only at the two places that need it:
// the Agent config response and the Controller's private readiness probe. The
// database stores only JWT-secret-encrypted material; the in-memory ControlNode
// never exposes this value through JSON.
func dNodeProbeSecret(db *DB, node ControlNode) (string, error) {
	if db == nil || db.db == nil || node.ID <= 0 {
		return "", errors.New("node probe secret is unavailable")
	}
	if strings.TrimSpace(node.probeSecretCiphertext) != "" {
		return decryptNodeProbeSecretWithSecret(node.probeSecretCiphertext, jwtSecret)
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
