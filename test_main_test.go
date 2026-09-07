package main

import (
	"os"
	"strings"
	"testing"
)

// Agent node tests need a stable signing key because production deliberately
// rejects persistent node credentials when JWT_SECRET is ephemeral. Individual
// tests that exercise ephemeral behavior still override the globals explicitly.
func TestMain(m *testing.M) {
	jwtSecret = []byte(strings.Repeat("test-jwt-secret-", 3))
	jwtSecretEphemeral = false
	os.Exit(m.Run())
}
