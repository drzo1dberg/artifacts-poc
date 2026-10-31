package main

import (
	"strings"
	"testing"
	"time"
)

func TestVerifyToken(t *testing.T) {
	key := []byte("test-key")
	now := time.Unix(1000, 0)
	claims := contentClaims{Artifact: "a", Owner: "alice", Version: 2, Expires: 1060}
	valid := signToken(key, claims)

	got, ok := verifyToken(key, valid, now)
	if !ok || got != claims {
		t.Fatalf("valid token rejected: %+v, %v", got, ok)
	}

	forged, _, _ := strings.Cut(signToken(key, contentClaims{Artifact: "a", Owner: "bob", Version: 2, Expires: 1060}), ".")
	_, sig, _ := strings.Cut(valid, ".")

	tests := []struct {
		name  string
		token string
		key   []byte
		now   time.Time
	}{
		{"expired", valid, key, time.Unix(1060, 0)},
		{"wrong key", valid, []byte("other-key"), now},
		{"payload swapped under a valid signature", forged + "." + sig, key, now},
		{"no signature", forged, key, now},
		{"empty", "", key, now},
		{"signature only", "." + sig, key, now},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := verifyToken(tt.key, tt.token, tt.now); ok {
				t.Fatal("token was accepted")
			}
		})
	}
}
