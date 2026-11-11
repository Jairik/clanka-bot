package tool

import (
	"crypto/rand"
	"encoding/hex"
)

// NewRequestID returns 16 random bytes as hex for correlation in logs. It is
// shared by the typed debug entry and the voice loop.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(b[:])
}
