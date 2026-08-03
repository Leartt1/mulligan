package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func passthrough() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	})
}

func TestRequireTokenAcceptsTheRightToken(t *testing.T) {
	h := RequireToken("s3cret", passthrough())

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestRequireTokenRefusesAMissingOrWrongToken(t *testing.T) {
	h := RequireToken("s3cret", passthrough())

	for name, header := range map[string]string{
		"absent":     "",
		"wrong":      "Bearer guess",
		"unprefixed": "s3cret",
		"empty":      "Bearer ",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s token: status = %d, want 401", name, rec.Code)
		}
		if rec.Body.String() == `{"ok": true}` {
			t.Errorf("%s token: the request reached the handler", name)
		}
	}
}

// The store is an unencrypted partial copy of production rows. A refusal that
// echoed the token, or said which part of it was wrong, would put it somewhere
// it does not belong — a proxy log, a screenshot in a ticket.
func TestRequireTokenNeverEchoesTheToken(t *testing.T) {
	h := RequireToken("s3cret", passthrough())

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer wrong-but-close")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String() + rec.Header().Get("WWW-Authenticate")
	if strings.Contains(body, "s3cret") || strings.Contains(body, "wrong-but-close") {
		t.Errorf("the refusal carries a token:\n%s", body)
	}
}

// An empty token means no token was configured, and wrapping in that state would
// lock everything out while looking like security.
func TestRequireTokenWithoutATokenIsAProgrammingError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("wrapping with an empty token was accepted")
		}
	}()

	RequireToken("", passthrough())
}
