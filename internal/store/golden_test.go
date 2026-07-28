package store

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

// The store outlives the process that wrote it, and once the source's binlogs
// rotate it is the only record of those changes. So the bytes on disk are a
// contract with every future build, not an implementation detail.
//
// Round-trip tests cannot enforce that contract: encode and decode move
// together, so swapping two tag values, flipping endianness, or reordering the
// column flags leaves every round-trip green while making the new build read old
// rows as different values. CodecVersion exists to catch exactly that, but it is
// bumped by hand and nothing would have failed to prompt it.
//
// These are hand-derived byte strings. If one of them fails, the format changed:
// either restore it, or change it deliberately and bump CodecVersion so old rows
// are refused rather than silently misread.
func TestEncodedRowBytesAreTheDocumentedFormat(t *testing.T) {
	tests := []struct {
		name string
		row  []any
		want string // hex
	}{
		{
			name: "version byte then count then one null",
			row:  []any{nil},
			want: "01" + "01" + "00",
		},
		{
			name: "small positive int is zigzag varint under tagInt",
			row:  []any{int64(1)},
			want: "01" + "01" + "01" + "02",
		},
		{
			name: "negative one",
			row:  []any{int64(-1)},
			want: "01" + "01" + "01" + "01",
		},
		{
			name: "unsigned max is a plain uvarint under tagUint",
			row:  []any{uint64(18446744073709551615)},
			want: "01" + "01" + "02" + "ffffffffffffffffff01",
		},
		{
			name: "bool true is a single byte under tagBool",
			row:  []any{true},
			want: "01" + "01" + "05" + "01",
		},
		{
			name: "bool false",
			row:  []any{false},
			want: "01" + "01" + "05" + "00",
		},
		{
			name: "string is length prefixed under tagString",
			row:  []any{"ab"},
			want: "01" + "01" + "06" + "02" + "6162",
		},
		{
			name: "bytes are length prefixed under tagBytes",
			row:  []any{[]byte{0xde, 0xad}},
			want: "01" + "01" + "07" + "02" + "dead",
		},
		{
			name: "raw is length prefixed under tagRaw",
			row:  []any{change.Raw("1.5")},
			want: "01" + "01" + "09" + "03" + "312e35",
		},
		{
			name: "empty row still carries the version and a zero count",
			row:  []any{},
			want: "01" + "00",
		},
		{
			name: "several values keep their order",
			row:  []any{int64(1), "a"},
			want: "01" + "02" + "01" + "02" + "06" + "01" + "61",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EncodeRow(tt.row)
			if err != nil {
				t.Fatalf("EncodeRow returned error: %v", err)
			}
			want, err := hex.DecodeString(tt.want)
			if err != nil {
				t.Fatalf("bad want literal: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("EncodeRow(%#v) =\n  %x\nwant\n  %x", tt.row, got, want)
			}
		})
	}
}

// Float width is the one collapse the codec must not make, so its bytes are
// pinned separately and explicitly. A float32 read back as float64 renders
// 0.10000000149011612 where the source column held 0.1.
func TestEncodedFloatBytesKeepTheirWidth(t *testing.T) {
	f32, err := EncodeRow([]any{float32(0.1)})
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}
	f64, err := EncodeRow([]any{float64(0.1)})
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}

	// Different tags, and 4 payload bytes against 8.
	if wantHex := "010103cdcccc3d"; hex.EncodeToString(f32) != wantHex {
		t.Errorf("float32 encoding = %x, want %s", f32, wantHex)
	}
	if wantHex := "0101049a9999999999b93f"; hex.EncodeToString(f64) != wantHex {
		t.Errorf("float64 encoding = %x, want %s", f64, wantHex)
	}
}

// Time is stored as an instant, so the bytes must not vary with the zone the
// value happens to carry. Two times naming the same moment in different zones
// have to encode identically, or a store written by a collector in one zone
// reads back differently in another.
func TestEncodedTimeBytesAreTheSameInstantRegardlessOfZone(t *testing.T) {
	utc := time.Date(2026, 7, 27, 13, 45, 5, 123456000, time.UTC)
	cest := utc.In(time.FixedZone("CEST", 2*60*60))

	a, err := EncodeRow([]any{utc})
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}
	b, err := EncodeRow([]any{cest})
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}

	if !bytes.Equal(a, b) {
		t.Errorf("the same instant encoded differently by zone:\n  %x\n  %x", a, b)
	}
}

// Column flags are packed as bits, so their values are as much a wire format as
// the value tags are. Swapping them would silently move the mark that keeps a
// reversal from assigning to a generated column.
func TestEncodedColumnBytesAreTheDocumentedFormat(t *testing.T) {
	tests := []struct {
		name string
		cols []change.Column
		want string
	}{
		{
			name: "plain column has no flags",
			cols: []change.Column{{Name: "id"}},
			want: "01" + "01" + "02" + "6964" + "00",
		},
		{
			name: "primary key is bit one",
			cols: []change.Column{{Name: "id", PrimaryKey: true}},
			want: "01" + "01" + "02" + "6964" + "01",
		},
		{
			name: "read only is bit two",
			cols: []change.Column{{Name: "id", ReadOnly: true}},
			want: "01" + "01" + "02" + "6964" + "02",
		},
		{
			name: "both flags set",
			cols: []change.Column{{Name: "id", PrimaryKey: true, ReadOnly: true}},
			want: "01" + "01" + "02" + "6964" + "03",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EncodeColumns(tt.cols)
			if err != nil {
				t.Fatalf("EncodeColumns returned error: %v", err)
			}
			want, err := hex.DecodeString(tt.want)
			if err != nil {
				t.Fatalf("bad want literal: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("EncodeColumns(%#v) =\n  %x\nwant\n  %x", tt.cols, got, want)
			}
		})
	}
}

// A count is read before anything is allocated for it, so a record claiming an
// absurd number of values must be refused on the length of the record rather
// than believed. Without the check a three-byte record reserves a multi-terabyte
// backing array and stalls the collector holding the only copy of the window.
func TestDecodeRefusesACountLargerThanTheRecord(t *testing.T) {
	// version 1, then a uvarint count of 1<<40, then nothing.
	absurd := append([]byte{CodecVersion}, 0x80, 0x80, 0x80, 0x80, 0x20)

	t.Run("row", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			if got, err := DecodeRow(absurd); err == nil {
				t.Errorf("DecodeRow accepted an absurd count, returning %d values", len(got))
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("DecodeRow did not return promptly, so it sized an allocation from the claimed count")
		}
	})

	t.Run("columns", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			if got, err := DecodeColumns(absurd); err == nil {
				t.Errorf("DecodeColumns accepted an absurd count, returning %d columns", len(got))
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("DecodeColumns did not return promptly, so it sized an allocation from the claimed count")
		}
	})
}
