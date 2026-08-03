package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/store"
)

// Change is one row change as a timeline renders it.
//
// Without its row images: a timeline of ten thousand rows should not carry ten
// thousand row images, and the detail route is one request away for the one a
// human actually opens.
type Change struct {
	ID           int64  `json:"id"`
	At           string `json:"at"`
	Schema       string `json:"schema"`
	Table        string `json:"table"`
	Op           string `json:"op"`
	LogFile      string `json:"log_file"`
	LogPos       uint32 `json:"log_pos"`
	Query        string `json:"query"`
	SchemaChange bool   `json:"schema_change"`
}

// ChangePage is one page of a timeline. Next is the cursor to pass as `before`
// for the following page, and is absent once there is nothing after it — a
// cursor that never stops being offered is one a client follows forever.
type ChangePage struct {
	Changes []Change `json:"changes"`
	Next    *int64   `json:"next,omitempty"`
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter, err := filterFromQuery(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := intParam(q, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := intParam(q, "before")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	entries, err := s.db.Page(filter, before, int(limit), s.now())
	if err != nil {
		// The store refuses a window it cannot answer for rather than returning
		// fewer rows, so an error here is usually a statement about coverage. It is
		// a conflict between what was asked for and what the store holds — not a
		// malformed request, and emphatically not a 200 with an empty array, which
		// is an affirmative "nothing happened" delivered during an incident.
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	page := ChangePage{Changes: make([]Change, 0, len(entries))}
	for _, e := range entries {
		page.Changes = append(page.Changes, newChange(e))
	}

	// A full page might be the last one; the next request returning nothing is how
	// that is discovered. Offering the cursor only when the page filled means one
	// wasted request at the end rather than a client that cannot tell it is done.
	if len(entries) > 0 && len(entries) == effectiveLimit(int(limit)) {
		next := entries[len(entries)-1].ID
		page.Next = &next
	}

	writeJSON(w, http.StatusOK, page)
}

func newChange(e store.Entry) Change {
	return Change{
		ID:           e.ID,
		At:           e.Event.At.UTC().Format(time.RFC3339),
		Schema:       e.Event.Schema,
		Table:        e.Event.Table,
		Op:           e.Event.Op.String(),
		LogFile:      e.Event.LogFile,
		LogPos:       e.Event.LogPos,
		Query:        e.Event.Query,
		SchemaChange: e.Event.IsSchemaChange(),
	}
}

// effectiveLimit mirrors what the store will do with a limit, so the cursor is
// offered exactly when a page came back full.
func effectiveLimit(limit int) int {
	switch {
	case limit <= 0:
		return store.DefaultPageSize
	case limit > store.MaxPageSize:
		return store.MaxPageSize
	}
	return limit
}

// filterFromQuery reads the window an operator asked about.
//
// Times go through the same parser the CLI uses, so "2026-07-25 17:20:00" and a
// bare "15:04" mean here what they mean there.
func filterFromQuery(q url.Values) (change.Filter, error) {
	f := change.Filter{}

	for _, name := range strings.Split(q.Get("tables"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			f.Tables = append(f.Tables, name)
		}
	}

	var err error
	if raw := q.Get("from"); raw != "" {
		if f.From, err = change.ParseTime(raw); err != nil {
			return f, fmt.Errorf("from: %w", err)
		}
	}
	if raw := q.Get("to"); raw != "" {
		if f.To, err = change.ParseTime(raw); err != nil {
			return f, fmt.Errorf("to: %w", err)
		}
	}

	// An inverted window matches nothing, and an empty result reads as "there was
	// nothing to undo" — the most misleading answer this tool can give.
	if !f.From.IsZero() && !f.To.IsZero() && f.To.Before(f.From) {
		return f, fmt.Errorf("to (%s) is before from (%s), which can never match a change",
			f.To.Format(time.RFC3339), f.From.Format(time.RFC3339))
	}
	return f, nil
}

// intParam reads a numeric parameter, treating absent as zero and unreadable as
// an error. A limit of "lots" silently becoming the default would answer a
// different question than the one asked.
func intParam(q url.Values, name string) (int64, error) {
	raw := q.Get(name)
	if raw == "" {
		return 0, nil
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", name, raw)
	}
	return n, nil
}
