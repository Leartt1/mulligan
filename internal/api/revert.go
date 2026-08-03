package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
	"github.com/learttytyri/mulligan/internal/script"
)

// handleRevert streams the script that undoes a window.
//
// It proposes and nothing more: no route here executes SQL against the watched
// database. The response is a file to review, which is the same contract the
// command line has always had.
func (s *Server) handleRevert(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter, err := filterFromQuery(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var generated []string
	for _, name := range strings.Split(q.Get("generated"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			generated = append(generated, name)
		}
	}

	now := s.now()

	// Two passes, and the first one is what makes the refusal safe. Schema changes
	// have to be known before the first statement is written, because the warning
	// sits above them — and a pass that reads the window is also a pass that
	// applies the coverage checks, so a store that cannot answer says so while the
	// response is still a status code rather than a half-written file.
	var schemaChanges []change.Event
	err = s.db.EachEvent(filter, now, func(ev change.Event) error {
		if ev.IsSchemaChange() {
			schemaChanges = append(schemaChanges, ev)
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="mulligan-revert.sql"`)
	w.WriteHeader(http.StatusOK)

	out := script.NewWriter(w)
	out.Open(s.label, filter, schemaChanges)

	err = s.db.EachEvent(filter, now, func(ev change.Event) error {
		if ev.IsSchemaChange() {
			return nil
		}

		one := []change.Event{ev}
		change.MarkReadOnly(one, generated)

		stmt, err := reverse.Statement(one[0])
		if err != nil {
			return err
		}
		return out.Write(reverse.Reversal{Event: one[0], Statement: stmt})
	})
	if err != nil {
		// The status line is long gone, so this cannot become a 500. The script is
		// left without its trailer and carries the reason instead: a truncated
		// script is detectable, where one that ends early and looks finished is the
		// thing that gets run.
		fmt.Fprintf(w, "\n-- ERROR: this script is incomplete and must not be run: %v\n", err)
		return
	}

	// The trailer is the completeness marker. Failing to write it leaves the
	// script visibly unfinished, which is the right outcome for a response that
	// could not be finished.
	if err := out.Close(); err != nil {
		fmt.Fprintf(w, "\n-- ERROR: this script is incomplete and must not be run: %v\n", err)
	}
}
