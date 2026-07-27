package binlog

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/shopspring/decimal"

	"github.com/learttytyri/mulligan/internal/change"
)

// The scanner decodes DECIMAL exactly rather than through float64, which means
// the value arriving here is a decimal.Decimal. Left as-is the engine has no
// rendering for it and refuses the whole revert; converted to a float it would
// round an amount. Carrying the exact text through avoids both.
func TestConvertCarriesDecimalThroughExactly(t *testing.T) {
	amount, err := decimal.NewFromString("1234.56")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	tm := &replication.TableMapEvent{
		Schema:      []byte("shop"),
		Table:       []byte("orders"),
		ColumnCount: 2,
		ColumnName:  [][]byte{[]byte("id"), []byte("total")},
		PrimaryKey:  []uint64{0},
	}
	rows := &replication.RowsEvent{
		Table:       tm,
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), amount}},
	}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}

	if want := change.Raw("1234.56"); got[0].After[1] != want {
		t.Errorf("total = %#v, want %#v", got[0].After[1], want)
	}
}

// A pointer to a decimal is what arrives for a nullable DECIMAL column, and
// falling through to the default would leave it unrenderable.
func TestConvertCarriesDecimalPointerThroughExactly(t *testing.T) {
	amount, err := decimal.NewFromString("0.001")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), &amount}},
	}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}

	if want := change.Raw("0.001"); got[0].After[1] != want {
		t.Errorf("value = %#v, want %#v", got[0].After[1], want)
	}
}

// Values the scanner already represents faithfully must not be disturbed.
func TestConvertLeavesOrdinaryValuesAlone(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "shipped"}},
	}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}

	if got[0].After[0] != int64(7) || got[0].After[1] != "shipped" {
		t.Errorf("row = %#v, want [7 shipped] unchanged", got[0].After)
	}
}
