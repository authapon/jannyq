package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

const (
	cookieName = "jannyq_session"
	idBytes    = 16
)

// signer issues and verifies session cookies of the form <id>.<signature>.
// The id is random; the signature (HMAC-SHA256 over it) means the server
// needs no session table and cannot be handed ids it never issued. The key
// also depends on the access code, so changing the code signs everyone out.
type signer struct {
	key []byte
}

func newSigner(secret []byte, accessCode string) *signer {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("jannyq-web-cookie-key\x00" + accessCode))
	return &signer{key: m.Sum(nil)}
}

func (s *signer) sign(id string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// issue creates a new session and returns its cookie value and id.
func (s *signer) issue() (value, id string) {
	var b [idBytes]byte
	_, _ = rand.Read(b[:])
	id = hex.EncodeToString(b[:])
	return id + "." + s.sign(id), id
}

// verify returns the session id of a cookie value, or "" when it is invalid.
func (s *signer) verify(value string) string {
	id, sig, ok := strings.Cut(value, ".")
	if !ok || len(id) != idBytes*2 {
		return ""
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(id))) {
		return ""
	}
	return id
}
