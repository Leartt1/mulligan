package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/learttytyri/mulligan/internal/reverse"
	"github.com/learttytyri/mulligan/internal/store"
)

// ChangeDetail is one change with the row images a diff is made of.
type ChangeDetail struct {
	Change

	Columns []DetailColumn `json:"columns"`

	// Before and After are indexed in step with Columns, and null where the
	// operation has no such image — before on an INSERT, after on a DELETE. An
	// empty array there would say the row existed with no columns.
	//
	// Each value is a string or null. Never a JSON number: every browser parses
	// one as a float64, so a DECIMAL or a BIGINT past 2^53 would be displayed
	// almost-right in the diff someone approves before running a statement.
	Before []*string `json:"before"`
	After  []*string `json:"after"`
}

// DetailColumn describes one column of the row, in image order.
type DetailColumn struct {
	Name       string `json:"name"`
	PrimaryKey bool   `json:"primary_key"`
	ReadOnly   bool   `json:"read_only"`
}

func (s *Server) handleChangeDetail(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("id: %q is not a number", raw))
		return
	}

	// Without a coverage check, deliberately. One stored change cannot be
	// silently incomplete the way a window can, and refusing to show it because
	// the collector has since stopped would withhold the one thing still visible
	// at the moment it is most wanted. store.Change carries the same reasoning.
	entry, ok, err := s.db.Change(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no change %d in this store", id))
		return
	}

	detail, err := newChangeDetail(entry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func newChangeDetail(e store.Entry) (ChangeDetail, error) {
	d := ChangeDetail{Change: newChange(e)}

	d.Columns = make([]DetailColumn, len(e.Event.Columns))
	for i, c := range e.Event.Columns {
		d.Columns[i] = DetailColumn{Name: c.Name, PrimaryKey: c.PrimaryKey, ReadOnly: c.ReadOnly}
	}

	var err error
	if d.Before, err = renderRow(e.Event.Before); err != nil {
		return d, fmt.Errorf("api: change %d before image: %w", e.ID, err)
	}
	if d.After, err = renderRow(e.Event.After); err != nil {
		return d, fmt.Errorf("api: change %d after image: %w", e.ID, err)
	}
	return d, nil
}

// renderRow renders a row image for display, or nil if the operation has none.
//
// Rendering goes through reverse.Display so the preview and the statement it
// previews cannot disagree about what a value is — the preview is what earns
// approval for the statement.
func renderRow(row []any) ([]*string, error) {
	if row == nil {
		return nil, nil
	}

	out := make([]*string, len(row))
	for i, v := range row {
		if v == nil {
			continue // JSON null: the column held no value
		}
		text, err := reverse.Display(v)
		if err != nil {
			return nil, err
		}
		out[i] = &text
	}
	return out, nil
}
