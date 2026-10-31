package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// contentClaims says who may read which version until when. Only the API can sign one,
// and it only does so for the logged-in owner, so a valid token proves a session existed.
type contentClaims struct {
	Artifact string `json:"a"`
	Owner    string `json:"o"`
	Version  int    `json:"v"`
	Expires  int64  `json:"e"`
}

// signToken returns <payload>.<signature>, both base64url, so it fits into one path segment.
// It has to live in the path: relative URLs inherit the path, so style.css and app.js carry it
// automatically. A query string would be lost, and sandboxed documents do not send cookies reliably.
func signToken(key []byte, c contentClaims) string {
	data, err := json.Marshal(c)
	if err != nil {
		panic(err) // contentClaims only holds strings and numbers
	}
	payload := base64.RawURLEncoding.EncodeToString(data)
	return payload + "." + signature(key, payload)
}

func verifyToken(key []byte, token string, now time.Time) (contentClaims, bool) {
	payload, sig, found := strings.Cut(token, ".")
	if !found || !hmac.Equal([]byte(sig), []byte(signature(key, payload))) {
		return contentClaims{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return contentClaims{}, false
	}
	var c contentClaims
	if err := json.Unmarshal(data, &c); err != nil || now.Unix() >= c.Expires {
		return contentClaims{}, false
	}
	return c, true
}

func signature(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
