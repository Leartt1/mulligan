package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/learttytyri/mulligan/internal/store"
)

// Server answers questions about one window store.
//
// It holds no state of its own beyond the store and the clock: every request is
// answered from what the store holds at that moment, because a collector is
// writing to it while this is serving and a cached answer would describe a
// window that has since moved.
type Server struct {
	db    *store.Store
	label string
	now   func() time.Time
	mux   *http.ServeMux
}

// New builds a server over db. The label is what reports call the store — its
// path, ordinarily — and now is injected so tests can state the moment they are
// asking about rather than racing the wall clock.
func New(db *store.Store, label string, now func() time.Time) *Server {
	s := &Server{db: db, label: label, now: now, mux: http.NewServeMux()}

	// Only GET is registered. Every route here reads; a surface that accepted
	// writes would invite one to be added without the decision being made again,
	// and "the tool proposes, a human runs it" is the whole safety model.
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/changes", s.handleChanges)
	s.mux.HandleFunc("GET /api/changes/{id}", s.handleChangeDetail)
	s.mux.HandleFunc("GET /api/revert.sql", s.handleRevert)

	// Anything unrouted answers in the same shape as everything else, rather than
	// in net/http's plain text — a client parsing JSON should not have to handle
	// one response that is not.
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such route: "+r.URL.Path)
	})

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The method is checked before routing. Left to the mux, a write to a known
	// path would fall through to the catch-all and come back as "no such route",
	// which misdescribes the refusal: the route exists, and it does not accept
	// writes because nothing here has a side effect.
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "this API is read-only; "+r.Method+" is not accepted on any route")
		return
	}

	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	report, err := s.db.Status(s.now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 200 whether or not the store is healthy: the report is the answer, and its
	// healthy field carries the verdict. A 5xx here would tell a monitoring
	// client the report failed when what failed is the thing being reported on.
	writeJSON(w, http.StatusOK, NewStatus(s.label, report))
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		// The status line and headers are already gone, so nothing can be said to
		// the client now. Breaking the JSON is deliberate: a truncated body is
		// detectable, where a body that ends early but parses is not.
		fmt.Fprint(w, "\n{\"error\": \"the response could not be written in full\"}\n")
	}
}

// errorBody is what every failure looks like, so a client has one shape to
// parse rather than one per route.
type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, errorBody{Error: message})
}
