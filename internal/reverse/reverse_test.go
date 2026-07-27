package reverse

import (
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/change"
)

func TestStatementRevertsInsertWithDelete(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "orders",
		Op:     change.Insert,
		Columns: []change.Column{
			{Name: "id", PrimaryKey: true},
			{Name: "status"},
		},
		After: []any{int64(7), "shipped"},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "DELETE FROM `shop`.`orders` WHERE `id` = 7 LIMIT 1;"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

func TestStatementRevertsDeleteWithInsertOfOldRow(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "orders",
		Op:     change.Delete,
		Columns: []change.Column{
			{Name: "id", PrimaryKey: true},
			{Name: "status"},
		},
		Before: []any{int64(7), "pending"},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "INSERT INTO `shop`.`orders` (`id`, `status`) VALUES (7, 'pending');"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

// An UPDATE is undone by restoring the before image, and only the columns that
// actually changed are touched — rewriting untouched columns would clobber
// concurrent writes to them.
func TestStatementRevertsUpdateRestoringOnlyChangedColumns(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "orders",
		Op:     change.Update,
		Columns: []change.Column{
			{Name: "id", PrimaryKey: true},
			{Name: "status"},
			{Name: "total"},
		},
		Before: []any{int64(7), "pending", int64(100)},
		After:  []any{int64(7), "shipped", int64(100)},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

// The row is located by its current (after-image) key, since that is what sits
// in the database now — using the before-image key would match nothing when the
// statement changed the key itself.
func TestStatementRevertsUpdateThatChangedThePrimaryKey(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "orders",
		Op:     change.Update,
		Columns: []change.Column{
			{Name: "id", PrimaryKey: true},
			{Name: "status"},
		},
		Before: []any{int64(7), "pending"},
		After:  []any{int64(9), "pending"},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "UPDATE `shop`.`orders` SET `id` = 7 WHERE `id` = 9 LIMIT 1;"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

// A generated column is logged like any other, but the database computes it and
// rejects any attempt to assign one. Restoring the columns it derives from is
// what brings it back.
func TestStatementDoesNotAssignToAReadOnlyColumnOnUpdate(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "invoices",
		Op:     change.Update,
		Columns: []change.Column{
			{Name: "id", PrimaryKey: true},
			{Name: "net"},
			{Name: "tax", ReadOnly: true},
		},
		Before: []any{int64(1), int64(100), int64(20)},
		After:  []any{int64(1), int64(999), int64(199)},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "UPDATE `shop`.`invoices` SET `net` = 100 WHERE `id` = 1 LIMIT 1;"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

// The same applies to re-inserting a deleted row: naming a generated column in
// the column list is rejected outright.
func TestStatementOmitsReadOnlyColumnsFromInsert(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "invoices",
		Op:     change.Delete,
		Columns: []change.Column{
			{Name: "id", PrimaryKey: true},
			{Name: "net"},
			{Name: "tax", ReadOnly: true},
		},
		Before: []any{int64(1), int64(100), int64(20)},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "INSERT INTO `shop`.`invoices` (`id`, `net`) VALUES (1, 100);"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

// Naming too many columns as generated is an easy mistake, and the resulting
// refusal has to point at that rather than claim the update changed nothing.
func TestStatementSaysWhenEveryChangedColumnIsReadOnly(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "invoices",
		Op:     change.Update,
		Columns: []change.Column{
			{Name: "id", PrimaryKey: true},
			{Name: "tax", ReadOnly: true},
		},
		Before: []any{int64(1), int64(20)},
		After:  []any{int64(1), int64(199)},
	}

	got, err := Statement(ev)
	if err == nil {
		t.Fatalf("Statement() = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error = %v, want it to point at the read-only columns", err)
	}
}

// Reading a generated column is fine — only assigning is refused — so it still
// helps identify a row in a table that has no primary key.
func TestStatementStillMatchesOnReadOnlyColumnsInWhere(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "invoices",
		Op:     change.Insert,
		Columns: []change.Column{
			{Name: "net"},
			{Name: "tax", ReadOnly: true},
		},
		After: []any{int64(100), int64(20)},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "DELETE FROM `shop`.`invoices` WHERE `net` = 100 AND `tax` = 20 LIMIT 1;"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

// Without a primary key there is no short way to name the row, so the full row
// image becomes the predicate. LIMIT 1 keeps a reversal from removing every
// duplicate of an identical row.
func TestStatementMatchesFullRowWhenTableHasNoPrimaryKey(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "audit",
		Op:     change.Insert,
		Columns: []change.Column{
			{Name: "actor"},
			{Name: "action"},
		},
		After: []any{"ada", "login"},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "DELETE FROM `shop`.`audit` WHERE `actor` = 'ada' AND `action` = 'login' LIMIT 1;"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

// `col = NULL` is never true in SQL, so a predicate built that way silently
// matches nothing and the reversal quietly does nothing at all.
func TestStatementComparesNullWithIsNull(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "audit",
		Op:     change.Insert,
		Columns: []change.Column{
			{Name: "actor"},
			{Name: "note"},
		},
		After: []any{"ada", nil},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "DELETE FROM `shop`.`audit` WHERE `actor` = 'ada' AND `note` IS NULL LIMIT 1;"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}

// A column set to NULL is restored with `= NULL` in SET, where assignment —
// unlike comparison — is the correct form.
func TestStatementRestoresNullValueInUpdate(t *testing.T) {
	ev := change.Event{
		Schema: "shop",
		Table:  "orders",
		Op:     change.Update,
		Columns: []change.Column{
			{Name: "id", PrimaryKey: true},
			{Name: "note"},
		},
		Before: []any{int64(7), nil},
		After:  []any{int64(7), "shipped late"},
	}

	got, err := Statement(ev)
	if err != nil {
		t.Fatalf("Statement returned error: %v", err)
	}

	want := "UPDATE `shop`.`orders` SET `note` = NULL WHERE `id` = 7 LIMIT 1;"
	if got != want {
		t.Errorf("Statement() =\n  %s\nwant\n  %s", got, want)
	}
}
