package auth

import (
	"crypto/rand"
	"encoding/hex"
)

// NewToken returns a random 32-byte token, hex-encoded — this is what goes
// in the session cookie. It's meaningless on its own; only the matching row
// in the sessions collection makes it useful, which is what lets logout
// revoke it server-side.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
