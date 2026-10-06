package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// RFC 4231, test case 2.
const (
	rfcKey  = "Jefe"
	rfcData = "what do ya want for nothing?"
	rfcHex  = "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
)

func TestVerifyHex(t *testing.T) {
	k, b := []byte(rfcKey), []byte(rfcData)
	for _, header := range []string{rfcHex, "sha256=" + rfcHex, " sha256=" + rfcHex + " ", strings.ToUpper(rfcHex)} {
		if !VerifyHex(k, b, header) {
			t.Errorf("valid signature %q rejected", header)
		}
	}
	for name, tc := range map[string]struct {
		secret, body []byte
		header       string
	}{
		"wrong body":     {k, []byte("tampered"), rfcHex},
		"wrong secret":   {[]byte("jefe"), b, rfcHex},
		"empty header":   {k, b, ""},
		"not hex":        {k, b, "zz" + rfcHex[2:]},
		"truncated":      {k, b, rfcHex[:60]},
		"wrong prefix":   {k, b, "sha1=" + rfcHex},
		"empty secret":   {nil, b, hex.EncodeToString(mac(nil, b))},
		"sig of nothing": {k, nil, rfcHex},
	} {
		if VerifyHex(tc.secret, tc.body, tc.header) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestVerifyBase64(t *testing.T) {
	raw, _ := hex.DecodeString(rfcHex)
	sig := base64.StdEncoding.EncodeToString(raw)
	if !VerifyBase64([]byte(rfcKey), []byte(rfcData), sig) || !VerifyBase64([]byte(rfcKey), []byte(rfcData), " "+sig+"\n") {
		t.Error("valid signature rejected")
	}
	for name, tc := range map[string]struct {
		secret, body []byte
		header       string
	}{
		"wrong body":   {[]byte(rfcKey), []byte("x"), sig},
		"wrong secret": {[]byte("k"), []byte(rfcData), sig},
		"hex instead":  {[]byte(rfcKey), []byte(rfcData), rfcHex},
		"empty":        {[]byte(rfcKey), []byte(rfcData), ""},
		"empty secret": {nil, []byte(rfcData), base64.StdEncoding.EncodeToString(mac(nil, []byte(rfcData)))},
	} {
		if VerifyBase64(tc.secret, tc.body, tc.header) {
			t.Errorf("%s accepted", name)
		}
	}
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestSignedHandler(t *testing.T) {
	var got []byte
	h := Signed(64, func(r *http.Request, body []byte) bool {
		return VerifyHex([]byte("s3cret"), body, r.Header.Get("X-Hub-Signature-256"))
	}, func(w http.ResponseWriter, r *http.Request, body []byte) {
		got = body
		w.WriteHeader(http.StatusOK)
	})
	do := func(body, sig string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/hook", strings.NewReader(body))
		if sig != "" {
			r.Header.Set("X-Hub-Signature-256", sig)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	if rec := do(`{"ok":1}`, sign("s3cret", `{"ok":1}`)); rec.Code != 200 || string(got) != `{"ok":1}` {
		t.Errorf("valid request: %d %q", rec.Code, got)
	}
	got = nil
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"no signature":    do(`{"ok":1}`, ""),
		"wrong signature": do(`{"ok":1}`, sign("other", `{"ok":1}`)),
		"body changed":    do(`{"ok":2}`, sign("s3cret", `{"ok":1}`)),
	} {
		if rec.Code != 401 || strings.Contains(rec.Body.String(), "s3cret") {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
	big := strings.Repeat("a", 200)
	if rec := do(big, sign("s3cret", big)); rec.Code != 413 {
		t.Errorf("oversized: %d", rec.Code)
	}
	if got != nil {
		t.Error("the handler ran for an invalid or oversized request")
	}
}

func TestMetaChallenge(t *testing.T) {
	h := MetaChallenge("verify-me")
	do := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/hook?"+q, nil))
		return rec
	}
	if rec := do("hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=12345"); rec.Code != 200 || rec.Body.String() != "12345" {
		t.Errorf("handshake: %d %q", rec.Code, rec.Body.String())
	}
	for _, q := range []string{
		"hub.mode=subscribe&hub.verify_token=nope&hub.challenge=1",
		"hub.mode=unsubscribe&hub.verify_token=verify-me&hub.challenge=1",
		"hub.challenge=1",
		"",
	} {
		if rec := do(q); rec.Code != 403 || rec.Body.Len() > 20 {
			t.Errorf("%q: %d %q", q, rec.Code, rec.Body.String())
		}
	}
	// a handler without a configured token refuses everything, even an empty token
	rec := httptest.NewRecorder()
	MetaChallenge("").ServeHTTP(rec, httptest.NewRequest("GET", "/?hub.mode=subscribe&hub.verify_token=&hub.challenge=1", nil))
	if rec.Code != 403 {
		t.Errorf("empty configured token accepted: %d", rec.Code)
	}
}

func TestDedupe(t *testing.T) {
	now := time.Unix(1000, 0)
	d := NewDedupe(time.Minute, 3)
	d.now = func() time.Time { return now }
	if d.Seen("a") || !d.Seen("a") {
		t.Error("first sighting must be new, second a duplicate")
	}
	if d.Seen("") || d.Seen("") {
		t.Error("empty keys are never duplicates")
	}
	now = now.Add(2 * time.Minute)
	if d.Seen("a") {
		t.Error("expired key must count as new")
	}
	// bounded memory
	for _, k := range []string{"b", "c", "d", "e", "f"} {
		d.Seen(k)
	}
	if len(d.seen) > 3 {
		t.Errorf("tracking %d keys, max is 3", len(d.seen))
	}
}
