package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireToken wraps h so every request must carry a bearer token.
//
// It exists for the case where the listener is not on loopback. What this store
// serves is an unencrypted partial copy of production rows, so binding it to an
// address other than 127.0.0.1 without something in front of it publishes them;
// a shared secret is the least this can insist on. There is no rotation, no
// expiry and no identity behind it — an audit trail could only ever say "someone
// holding the token" — which is recorded as a limit rather than dressed up.
//
// The comparison is constant time. The difference it defends against is small
// over a network, and the alternative is a comparison whose duration is a
// function of a secret, which is not worth keeping.
//
// An empty token panics: it means no token was configured, and wrapping in that
// state would refuse every request while looking like security was applied.
func RequireToken(token string, h http.Handler) http.Handler {
	if token == "" {
		panic("api: RequireToken needs a token; an empty one would refuse every request")
	}
	want := []byte(token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			// Nothing about the presented value is repeated back: a refusal that
			// echoed it would put a secret into a proxy log or a screenshot in a
			// ticket, and saying which part was wrong would help guess the rest.
			w.Header().Set("WWW-Authenticate", `Bearer realm="mulligan"`)
			writeError(w, http.StatusUnauthorized, "this listener requires a bearer token; set MULLIGAN_TOKEN and send it as Authorization: Bearer <token>")
			return
		}

		h.ServeHTTP(w, r)
	})
}
