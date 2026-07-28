package store

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
)

// renderValue returns the SQL the reverse engine renders for a single column
// value.
//
// literal() is unexported in package reverse, so a one-column DELETE event is
// the closest honest stand-in: reversing a DELETE feeds every value of the
// before image straight through literal to build the VALUES list. Going through
// the real renderer is the point — a codec test that compared decode against
// encode could pass while both agreed on something the renderer cannot use.
func renderValue(t *testing.T, v any) (string, error) {
	t.Helper()
	return reverse.Statement(change.Event{
		Schema:  "s",
		Table:   "t",
		Op:      change.Delete,
		Columns: []change.Column{{Name: "v"}},
		Before:  []any{v},
	})
}

// wantStatement wraps a hand-written literal in the statement the renderer
// builds around it, so the table below can stay readable while still pinning
// the exact SQL rather than whatever the code happens to produce.
func wantStatement(literal string) string {
	return "INSERT INTO `s`.`t` (`v`) VALUES (" + literal + ");"
}

// The window store is not a cache that can be rebuilt: once the source's
// binlogs rotate away it is the only surviving record of those changes. So the
// only definition of lossless that matters is that the SQL rendered from a
// decoded value is byte-identical to the SQL rendered from the original. A
// value that merely "looks equal" after a round trip but renders differently
// restores the wrong data, and nothing downstream would notice.
func TestDecodedValueRendersTheSameSQLAsTheOriginal(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string // the literal the reverse engine renders, written by hand
	}{
		{"nil is NULL, not the string 'nil'", nil, "NULL"},

		{"int64", int64(7), "7"},
		{"negative int64", int64(-42), "-42"},
		{"int64 min", int64(math.MinInt64), "-9223372036854775808"},

		// Every signed width is formatted through strconv.FormatInt by the
		// renderer, so collapsing them all onto one tag is invisible in the SQL.
		// These cases exist to prove that claim rather than assume it.
		{"plain int collapses to the same output", int(-42), "-42"},
		{"int8", int8(-128), "-128"},
		{"int16", int16(-32768), "-32768"},
		{"int32", int32(-2147483648), "-2147483648"},

		{"uint64 max survives without wrapping to -1", uint64(18446744073709551615), "18446744073709551615"},
		{"plain uint collapses to the same output", uint(42), "42"},
		{"uint8", uint8(255), "255"},
		{"uint16", uint16(65535), "65535"},
		{"uint32", uint32(4294967295), "4294967295"},

		// A float32 widened to float64 renders 0.10000000149011612, because the
		// renderer formats float32 with bitSize 32 and float64 with 64. That is a
		// silently wrong number written back into the column.
		{"float32 keeps its own width", float32(0.1), "0.1"},
		{"float64", float64(0.1), "0.1"},
		{"float64 without fraction is not truncated to int", float64(2), "2"},
		{"negative float32", float32(-1.5), "-1.5"},

		{"bool true is 1", true, "1"},
		{"bool false is 0", false, "0"},

		{"empty string", "", "''"},
		{"plain string", "pending", "'pending'"},
		{"single quote", "it's", "'it''s'"},
		{"backslash falls back to hex", `C:\tmp`, "X'433A5C746D70'"},
		{"non-UTF8 string falls back to hex", "h\xe9llo", "X'68E96C6C6F'"},
		{"multi-byte utf8 stays readable", "héllo → 🌍", "'héllo → 🌍'"},

		{"binary bytes are hex", []byte{0xde, 0xad, 0xbe, 0xef}, "X'DEADBEEF'"},
		{"bytes that read as text stay readable", []byte("plain text"), "'plain text'"},
		{"empty bytes", []byte{}, "''"},
		{"bytes with a NUL fall back to hex", []byte{0x61, 0x00, 0x62}, "X'610062'"},

		{"datetime", time.Date(2026, 7, 27, 13, 45, 5, 0, time.UTC), "'2026-07-27 13:45:05'"},
		{"datetime keeps microseconds", time.Date(2026, 7, 27, 13, 45, 5, 123456000, time.UTC), "'2026-07-27 13:45:05.123456'"},

		// Dates before 1970 are ordinary in a DATETIME column, and their unix
		// seconds are negative — a length or offset stored unsigned would mangle
		// them.
		{"datetime before the unix epoch", time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), "'1000-01-01 00:00:00'"},

		// A TIMESTAMP decodes into the reading machine's zone, and the renderer
		// normalizes to UTC. Storing the instant rather than the wall clock is
		// what keeps that true after a round trip through the store.
		{
			"timestamp in another zone stays the same instant",
			time.Date(2026, 7, 27, 15, 45, 5, 0, time.FixedZone("CEST", 2*60*60)),
			"'2026-07-27 13:45:05'",
		},

		// DECIMAL arrives as pre-rendered text precisely so no amount is rounded
		// through float64. Storing it as anything numeric would undo that.
		{"raw decimal", change.Raw("1234.5678"), "1234.5678"},
		{"raw negative decimal", change.Raw("-0.001"), "-0.001"},
		{"raw keeps the trailing zeros of its scale", change.Raw("10.00"), "10.00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := wantStatement(tt.want)

			before, err := renderValue(t, tt.in)
			if err != nil {
				t.Fatalf("rendering the original %#v returned error: %v", tt.in, err)
			}
			if before != want {
				t.Fatalf("original renders as\n  %s\nwant\n  %s", before, want)
			}

			b, err := EncodeRow([]any{tt.in})
			if err != nil {
				t.Fatalf("EncodeRow(%#v) returned error: %v", tt.in, err)
			}
			got, err := DecodeRow(b)
			if err != nil {
				t.Fatalf("DecodeRow returned error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("DecodeRow returned %d values, want 1", len(got))
			}

			after, err := renderValue(t, got[0])
			if err != nil {
				t.Fatalf("rendering the decoded %#v returned error: %v", got[0], err)
			}
			if after != want {
				t.Errorf("decoded value renders as\n  %s\nwant\n  %s", after, want)
			}
		})
	}
}

// The reverse engine decides which columns an UPDATE actually changed by
// comparing the two images with reflect.DeepEqual. A []byte that came back as a
// string, or a uint64 that came back as an int64, would compare unequal to an
// untouched counterpart and produce an assignment the statement never made.
func TestDecodedRowKeepsTypesThatComparisonDependsOn(t *testing.T) {
	row := []any{
		nil,
		int64(-42),
		uint64(18446744073709551615),
		float32(0.1),
		float64(0.1),
		true,
		false,
		"text",
		[]byte("bytes"),
		[]byte{},
		change.Raw("10.00"),
	}

	b, err := EncodeRow(row)
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}
	got, err := DecodeRow(b)
	if err != nil {
		t.Fatalf("DecodeRow returned error: %v", err)
	}

	if !reflect.DeepEqual(got, row) {
		t.Errorf("DecodeRow(EncodeRow(row)) = %#v, want %#v", got, row)
	}
}

// Widening a float32 on the way out renders 0.10000000149011612 instead of 0.1,
// which is a different number written back into the column. The tag has to
// carry the width.
func TestDecodedFloat32DoesNotWidenIntoFloat64(t *testing.T) {
	b, err := EncodeRow([]any{float32(0.1)})
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}
	got, err := DecodeRow(b)
	if err != nil {
		t.Fatalf("DecodeRow returned error: %v", err)
	}

	if _, ok := got[0].(float32); !ok {
		t.Fatalf("decoded value is %T, want float32", got[0])
	}
}

// The instant is what matters, not the zone the value happened to be read in:
// the renderer normalizes to UTC, and a stored offset would shift the written
// time by whatever offset the machine that read the log had.
func TestDecodedTimeIsTheSameInstantToTheNanosecond(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
	}{
		{"whole second", time.Date(2026, 7, 27, 13, 45, 5, 0, time.UTC)},
		{"microseconds", time.Date(2026, 7, 27, 13, 45, 5, 123456000, time.UTC)},
		{"nanoseconds", time.Date(2026, 7, 27, 13, 45, 5, 123456789, time.UTC)},
		{"in a zone east of UTC", time.Date(2026, 7, 27, 15, 45, 5, 1, time.FixedZone("CEST", 2*60*60))},
		{"before the unix epoch", time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := EncodeRow([]any{tt.in})
			if err != nil {
				t.Fatalf("EncodeRow returned error: %v", err)
			}
			got, err := DecodeRow(b)
			if err != nil {
				t.Fatalf("DecodeRow returned error: %v", err)
			}

			ts, ok := got[0].(time.Time)
			if !ok {
				t.Fatalf("decoded value is %T, want time.Time", got[0])
			}
			if !ts.Equal(tt.in) {
				t.Errorf("decoded time = %v, want the same instant as %v", ts, tt.in)
			}
			if ts.Nanosecond() != tt.in.Nanosecond() {
				t.Errorf("decoded nanoseconds = %d, want %d", ts.Nanosecond(), tt.in.Nanosecond())
			}
		})
	}
}

func TestEncodeRowRoundTripsAnEmptyRow(t *testing.T) {
	b, err := EncodeRow(nil)
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}
	got, err := DecodeRow(b)
	if err != nil {
		t.Fatalf("DecodeRow returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DecodeRow returned %#v, want no values", got)
	}
}

// A decoded []byte must not point into the buffer it was read from. SQLite
// hands back memory it is free to reuse, and a row image aliasing it would
// change under the engine between being read and being rendered.
func TestDecodedBytesDoNotAliasTheEncodedBuffer(t *testing.T) {
	b, err := EncodeRow([]any{[]byte("payload")})
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}
	got, err := DecodeRow(b)
	if err != nil {
		t.Fatalf("DecodeRow returned error: %v", err)
	}

	for i := range b {
		b[i] = 0xff
	}

	if !reflect.DeepEqual(got[0], []byte("payload")) {
		t.Errorf("decoded bytes changed with the buffer: %#v", got[0])
	}
}

// valueRow builds the encoding of a one-value row by hand, so the decoder can
// be fed bytes a well-behaved encoder would never produce. That is exactly the
// case that matters: a record on disk that was corrupted or edited.
func valueRow(tag byte, payload []byte) []byte {
	b := []byte{CodecVersion}
	b = binary.AppendUvarint(b, 1)
	b = append(b, tag)
	return append(b, payload...)
}

// rawRow builds the encoding of a single Raw value carrying s.
func rawRow(s string) []byte {
	return valueRow(tagRaw, append(binary.AppendUvarint(nil, uint64(len(s))), s...))
}

// change.Raw is the one value that reaches the generated script unquoted, and
// the renderer only lets numbers through. The store must apply the same rule on
// the way out: it is a file on disk, and an attacker who can edit it must not be
// able to reopen the unquoted-SQL hole in a script an operator then runs by hand.
func TestDecodeRefusesRawThatIsNotANumber(t *testing.T) {
	hostile := []string{
		"1; DROP TABLE orders",
		"1 OR 1=1",
		"(SELECT 1)",
		"NULL",
		"",
		"1'",
		"0x41",
		"1--",
		"1 ",
		"1e",
		".5",
	}

	for _, in := range hostile {
		t.Run(in, func(t *testing.T) {
			got, err := DecodeRow(rawRow(in))
			if err == nil {
				t.Errorf("DecodeRow of raw %q = %#v, want error", in, got)
			}
		})
	}
}

// The same grammar the renderer accepts has to survive storage, or reversing a
// DECIMAL change would start failing after a restart.
func TestDecodeAcceptsNumericRaw(t *testing.T) {
	fine := []string{"0", "-1", "1234.5678", "10.00", "-0.001", "1e10", "1.5E-3", "+7"}

	for _, in := range fine {
		t.Run(in, func(t *testing.T) {
			got, err := DecodeRow(rawRow(in))
			if err != nil {
				t.Fatalf("DecodeRow of raw %q returned error: %v", in, err)
			}
			if !reflect.DeepEqual(got, []any{change.Raw(in)}) {
				t.Errorf("DecodeRow of raw %q = %#v, want %#v", in, got, change.Raw(in))
			}
		})
	}
}

// Refusing at write time too means the bad value never reaches the disk, so the
// failure surfaces while the source is still readable rather than at reversal
// time when it may be the only copy left.
func TestEncodeRefusesRawThatIsNotANumber(t *testing.T) {
	if got, err := EncodeRow([]any{change.Raw("1; DROP TABLE orders")}); err == nil {
		t.Errorf("EncodeRow = %#v, want error", got)
	}
}

// A value the codec does not understand cannot be rendered, and storing it
// would only move the failure to the point where the binlog is gone.
func TestEncodeRefusesUnsupportedType(t *testing.T) {
	if got, err := EncodeRow([]any{struct{ A int }{1}}); err == nil {
		t.Errorf("EncodeRow = %#v, want error", got)
	}
}

// Skipping an unknown tag would shift every value after it onto the wrong
// column, and returning nil for it would write NULL over data that was never
// NULL. Both are silent; the error is not.
//
// Neither record below has a spare byte at the end, deliberately: an unknown tag
// that consumed nothing would otherwise be caught by the check for trailing
// bytes, and this test would pass while the tag itself was being ignored.
func TestDecodeRefusesUnknownTag(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"as the only value", []byte{CodecVersion, 0x01, 0xfe}},
		{"followed by a value the decoder does know", []byte{CodecVersion, 0x02, 0xfe, tagNull}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := DecodeRow(tt.in); err == nil {
				t.Errorf("DecodeRow(%#v) = %#v, want error for an unknown tag", tt.in, got)
			}
		})
	}
}

// A later format change must fail loudly here rather than be read as version 1
// and produce plausible wrong values from bytes that mean something else.
func TestDecodeRefusesAnUnknownCodecVersion(t *testing.T) {
	b, err := EncodeRow([]any{int64(7)})
	if err != nil {
		t.Fatalf("EncodeRow returned error: %v", err)
	}
	b[0] = CodecVersion + 1

	if got, err := DecodeRow(b); err == nil {
		t.Errorf("DecodeRow = %#v, want error for an unknown version", got)
	}
}

// A truncated or corrupt record is a routine consequence of a crash mid-write.
// Every one of these must come back as an error: a panic here takes down the
// process that is holding the only remaining copy of the window.
func TestDecodeRowRefusesTruncatedAndMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"version only", []byte{CodecVersion}},
		{"count promises a value that is not there", []byte{CodecVersion, 0x01}},
		{"count promises three values, one present", []byte{CodecVersion, 0x03, tagNull}},
		{"int tag with no payload", []byte{CodecVersion, 0x01, tagInt}},
		{"uint tag with no payload", []byte{CodecVersion, 0x01, tagUint}},
		{"bool tag with no payload", []byte{CodecVersion, 0x01, tagBool}},
		{"bool byte that is neither 0 nor 1", []byte{CodecVersion, 0x01, tagBool, 0x02}},
		{"float32 with three bytes", []byte{CodecVersion, 0x01, tagFloat32, 0x00, 0x00, 0x00}},
		{"float64 with seven bytes", []byte{CodecVersion, 0x01, tagFloat64, 0, 0, 0, 0, 0, 0, 0}},
		{"string length runs past the end", []byte{CodecVersion, 0x01, tagString, 0x64, 'a', 'b'}},
		{"bytes length runs past the end", []byte{CodecVersion, 0x01, tagBytes, 0x64, 'a', 'b'}},
		{"raw length runs past the end", []byte{CodecVersion, 0x01, tagRaw, 0x64, '1'}},

		// A length has to be checked against what is actually there before it is
		// used to size anything, or a corrupt record turns into an allocation
		// that kills the process.
		{"string length is a terabyte", valueRow(tagString, binary.AppendUvarint(nil, 1<<40))},
		{"string length overflows a uint64", valueRow(tagString, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})},

		{"time with seconds but no nanoseconds", []byte{CodecVersion, 0x01, tagTime, 0x02}},
		{"time with nanoseconds out of range", valueRow(tagTime, append(binary.AppendVarint(nil, 1), binary.AppendUvarint(nil, 1_000_000_000)...))},
		{"unterminated varint", []byte{CodecVersion, 0x01, tagInt, 0xff, 0xff, 0xff}},
		{"trailing bytes after the last value", []byte{CodecVersion, 0x01, tagNull, 0x00}},
		{"garbage", []byte{0xde, 0xad, 0xbe, 0xef}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeRow(tt.in)
			if err == nil {
				t.Errorf("DecodeRow(%#v) = %#v, want error", tt.in, got)
			}
		})
	}
}

// PrimaryKey decides how a reversal finds the row and ReadOnly decides whether
// it is assigned to at all. A flag lost in storage produces either a statement
// that matches the wrong rows or one the server rejects outright.
func TestEncodedColumnsKeepPrimaryKeyAndReadOnlyFlags(t *testing.T) {
	cols := []change.Column{
		{Name: "id", PrimaryKey: true},
		{Name: "net"},
		{Name: "tax", ReadOnly: true},
		{Name: "shard", PrimaryKey: true, ReadOnly: true},
		{Name: "odd `name`"},
		{Name: "héllo"},
		{Name: ""},
	}

	b, err := EncodeColumns(cols)
	if err != nil {
		t.Fatalf("EncodeColumns returned error: %v", err)
	}
	got, err := DecodeColumns(b)
	if err != nil {
		t.Fatalf("DecodeColumns returned error: %v", err)
	}

	want := []change.Column{
		{Name: "id", PrimaryKey: true, ReadOnly: false},
		{Name: "net", PrimaryKey: false, ReadOnly: false},
		{Name: "tax", PrimaryKey: false, ReadOnly: true},
		{Name: "shard", PrimaryKey: true, ReadOnly: true},
		{Name: "odd `name`", PrimaryKey: false, ReadOnly: false},
		{Name: "héllo", PrimaryKey: false, ReadOnly: false},
		{Name: "", PrimaryKey: false, ReadOnly: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecodeColumns(EncodeColumns(cols)) = %#v, want %#v", got, want)
	}
}

func TestEncodeColumnsRoundTripsAnEmptyList(t *testing.T) {
	b, err := EncodeColumns(nil)
	if err != nil {
		t.Fatalf("EncodeColumns returned error: %v", err)
	}
	got, err := DecodeColumns(b)
	if err != nil {
		t.Fatalf("DecodeColumns returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DecodeColumns returned %#v, want no columns", got)
	}
}

// The column list is what the row images are indexed against, so a corrupt one
// mislabels every value in the row. It has to be refused rather than guessed at.
func TestDecodeColumnsRefusesTruncatedAndMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"version only", []byte{CodecVersion}},
		{"count promises a column that is not there", []byte{CodecVersion, 0x01}},
		{"name length runs past the end", []byte{CodecVersion, 0x01, 0x64, 'i', 'd'}},
		{"name present but no flags byte", []byte{CodecVersion, 0x01, 0x02, 'i', 'd'}},
		{"unknown flag bit set", []byte{CodecVersion, 0x01, 0x02, 'i', 'd', 0x04}},
		{"trailing bytes after the last column", []byte{CodecVersion, 0x01, 0x02, 'i', 'd', 0x01, 0x00}},
		{"garbage", []byte{0xde, 0xad, 0xbe, 0xef}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeColumns(tt.in)
			if err == nil {
				t.Errorf("DecodeColumns(%#v) = %#v, want error", tt.in, got)
			}
		})
	}
}

func TestDecodeColumnsRefusesAnUnknownCodecVersion(t *testing.T) {
	b, err := EncodeColumns([]change.Column{{Name: "id", PrimaryKey: true}})
	if err != nil {
		t.Fatalf("EncodeColumns returned error: %v", err)
	}
	b[0] = CodecVersion + 1

	if got, err := DecodeColumns(b); err == nil {
		t.Errorf("DecodeColumns = %#v, want error for an unknown version", got)
	}
}
