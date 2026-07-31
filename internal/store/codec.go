// Package store persists the rolling window of row changes Mulligan has read
// from a source, so that the console can query recent activity by time, table
// and user without re-scanning a binary log. The window is not a cache that can
// be rebuilt on demand: a source expires its binlogs on its own schedule, and
// once it has, this store holds the only surviving record of the changes it
// covers. Everything here is written with that in mind — a value goes back out
// exactly as faithfully as it came in, or it does not go in at all.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

// CodecVersion is the encoding this package writes, stored as the first byte of
// every record so that a later format change fails loudly on old rows instead of
// reading their bytes as though they meant something else.
const CodecVersion = 1

// Value tags. The width of an integer is deliberately not among them: the
// reverse engine formats every signed width through strconv.FormatInt and every
// unsigned one through FormatUint, so int8 and int64 holding the same number
// render the same SQL and collapsing them costs nothing.
//
// Floats are the exception and must keep their width. The renderer formats a
// float32 with bitSize 32, so a float32(0.1) widened to float64 on the way out
// renders 0.10000000149011612 — a different number written back into the column.
const (
	tagNull byte = iota
	tagInt
	tagUint
	tagFloat32
	tagFloat64
	tagBool
	tagString
	tagBytes
	tagTime
	tagRaw
)

// Column flags, packed into one byte per column.
const (
	flagPrimaryKey byte = 1 << iota
	flagReadOnly

	// knownFlags is what this version understands. A bit outside it means the
	// record was not written by this encoder, and guessing at it could drop the
	// mark that keeps a reversal from assigning to a generated column.
	knownFlags = flagPrimaryKey | flagReadOnly
)

// errTruncated is what every bounds check reports. A record cut short is the
// ordinary result of a crash part way through a write, so it has to come back as
// an error rather than a panic: the process holding the only copy of the window
// is the last one that should be brought down by a bad row.
var errTruncated = errors.New("store: record ends mid-value")

// EncodeRow encodes a row image for storage.
//
// It refuses any value the reverse engine could not render. Storing one would
// only postpone the failure to the moment the reversal is needed, which is
// typically after the source binlog holding the original has expired.
func EncodeRow(row []any) ([]byte, error) {
	b := make([]byte, 0, 16+16*len(row))
	b = append(b, CodecVersion)
	b = binary.AppendUvarint(b, uint64(len(row)))

	for i, v := range row {
		var err error
		if b, err = appendValue(b, v); err != nil {
			return nil, fmt.Errorf("store: column %d: %w", i, err)
		}
	}
	return b, nil
}

// appendValue writes one tagged value. The cases mirror those the reverse
// engine's literal() accepts, since a value it cannot render has no business in
// the store.
func appendValue(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, tagNull), nil

	case bool:
		if x {
			return append(b, tagBool, 1), nil
		}
		return append(b, tagBool, 0), nil

	case int:
		return appendInt(b, int64(x)), nil
	case int8:
		return appendInt(b, int64(x)), nil
	case int16:
		return appendInt(b, int64(x)), nil
	case int32:
		return appendInt(b, int64(x)), nil
	case int64:
		return appendInt(b, x), nil

	case uint:
		return appendUint(b, uint64(x)), nil
	case uint8:
		return appendUint(b, uint64(x)), nil
	case uint16:
		return appendUint(b, uint64(x)), nil
	case uint32:
		return appendUint(b, uint64(x)), nil
	case uint64:
		return appendUint(b, x), nil

	case float32:
		b = append(b, tagFloat32)
		return binary.LittleEndian.AppendUint32(b, math.Float32bits(x)), nil
	case float64:
		b = append(b, tagFloat64)
		return binary.LittleEndian.AppendUint64(b, math.Float64bits(x)), nil

	case string:
		return appendLenPrefixed(b, tagString, x), nil

	// TEXT and BLOB are one type in the log, so both arrive as bytes. The
	// distinction from string is kept anyway: the reverse engine decides which
	// columns an UPDATE touched with reflect.DeepEqual, and a []byte that came
	// back as a string would compare unequal to an untouched counterpart and
	// produce an assignment the original statement never made.
	case []byte:
		return appendLenPrefixed(b, tagBytes, string(x)), nil

	// Only the instant is stored. A TIMESTAMP is read in whatever zone the
	// machine reading the log sits in and the renderer normalizes to UTC, so
	// keeping the wall clock instead would write back a time shifted by that
	// machine's offset.
	case time.Time:
		b = append(b, tagTime)
		b = binary.AppendVarint(b, x.Unix())
		return binary.AppendUvarint(b, uint64(x.Nanosecond())), nil

	// A Raw reaches the generated script unquoted, and the renderer lets only
	// numbers through. Applying the same rule here keeps the bad value out of the
	// file entirely, so the complaint arrives while the source can still be
	// consulted rather than at reversal time.
	case change.Raw:
		if !change.ValidRaw(string(x)) {
			return nil, fmt.Errorf("raw value %q is not a number; raw values are stored to be emitted unquoted and must not carry arbitrary SQL", string(x))
		}
		return appendLenPrefixed(b, tagRaw, string(x)), nil
	}

	return nil, fmt.Errorf("unsupported value type %T", v)
}

func appendInt(b []byte, v int64) []byte {
	return binary.AppendVarint(append(b, tagInt), v)
}

func appendUint(b []byte, v uint64) []byte {
	return binary.AppendUvarint(append(b, tagUint), v)
}

func appendLenPrefixed(b []byte, tag byte, s string) []byte {
	b = append(b, tag)
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// DecodeRow decodes a row image written by EncodeRow.
func DecodeRow(b []byte) ([]any, error) {
	r := reader{b: b}
	if err := r.checkVersion(); err != nil {
		return nil, err
	}

	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	// Every value costs at least its tag byte, so a count larger than what is
	// left cannot be honest — and must be caught before it is used to size an
	// allocation.
	if n > uint64(r.remaining()) {
		return nil, fmt.Errorf("store: record claims %d values with %d bytes left: %w", n, r.remaining(), errTruncated)
	}

	row := make([]any, 0, n)
	for i := uint64(0); i < n; i++ {
		v, err := r.value()
		if err != nil {
			return nil, fmt.Errorf("store: column %d: %w", i, err)
		}
		row = append(row, v)
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("store: %d bytes left after %d values; the record is not one this codec wrote", r.remaining(), n)
	}
	return row, nil
}

// EncodeColumns encodes the column list a row image is indexed against.
func EncodeColumns(cols []change.Column) ([]byte, error) {
	b := make([]byte, 0, 16+16*len(cols))
	b = append(b, CodecVersion)
	b = binary.AppendUvarint(b, uint64(len(cols)))

	for _, c := range cols {
		b = binary.AppendUvarint(b, uint64(len(c.Name)))
		b = append(b, c.Name...)

		var flags byte
		if c.PrimaryKey {
			flags |= flagPrimaryKey
		}
		if c.ReadOnly {
			flags |= flagReadOnly
		}
		b = append(b, flags)
	}
	return b, nil
}

// DecodeColumns decodes a column list written by EncodeColumns.
func DecodeColumns(b []byte) ([]change.Column, error) {
	r := reader{b: b}
	if err := r.checkVersion(); err != nil {
		return nil, err
	}

	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	// A column costs at least its length byte and its flags byte.
	if n > uint64(r.remaining()/2) {
		return nil, fmt.Errorf("store: record claims %d columns with %d bytes left: %w", n, r.remaining(), errTruncated)
	}

	cols := make([]change.Column, 0, n)
	for i := uint64(0); i < n; i++ {
		name, err := r.lenPrefixed()
		if err != nil {
			return nil, fmt.Errorf("store: column %d: %w", i, err)
		}
		flags, err := r.readByte()
		if err != nil {
			return nil, fmt.Errorf("store: column %d flags: %w", i, err)
		}
		if flags&^knownFlags != 0 {
			return nil, fmt.Errorf("store: column %d carries unknown flags 0x%02x", i, flags)
		}
		cols = append(cols, change.Column{
			Name:       string(name),
			PrimaryKey: flags&flagPrimaryKey != 0,
			ReadOnly:   flags&flagReadOnly != 0,
		})
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("store: %d bytes left after %d columns; the record is not one this codec wrote", r.remaining(), n)
	}
	return cols, nil
}

// reader walks an encoded record. Every read is bounds-checked here so that no
// caller has to remember to do it, and so a corrupt record can never index past
// the end of the buffer.
type reader struct {
	b []byte
	i int
}

func (r *reader) remaining() int { return len(r.b) - r.i }

func (r *reader) checkVersion() error {
	v, err := r.readByte()
	if err != nil {
		return fmt.Errorf("store: record is empty: %w", err)
	}
	// Older records stay readable: a store is not rebuildable, and once the
	// source's binlogs rotate it is the only record of what it holds. A newer one
	// is refused, because it may use a tag this build does not know and reading it
	// would turn something unrecognised into a plausible value.
	if v > CodecVersion {
		return fmt.Errorf("store: record was written by a newer build (codec version %d, this build reads %d)",
			v, CodecVersion)
	}
	return nil
}

func (r *reader) readByte() (byte, error) {
	if r.remaining() < 1 {
		return 0, errTruncated
	}
	c := r.b[r.i]
	r.i++
	return c, nil
}

func (r *reader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.b[r.i:])
	if n == 0 {
		return 0, errTruncated
	}
	if n < 0 {
		return 0, errors.New("store: encoded length or count does not fit in 64 bits")
	}
	r.i += n
	return v, nil
}

func (r *reader) varint() (int64, error) {
	v, n := binary.Varint(r.b[r.i:])
	if n == 0 {
		return 0, errTruncated
	}
	if n < 0 {
		return 0, errors.New("store: encoded integer does not fit in 64 bits")
	}
	r.i += n
	return v, nil
}

// next returns the next n bytes as a view into the record. Callers that keep the
// result beyond the call must copy it.
func (r *reader) next(n uint64) ([]byte, error) {
	if n > uint64(r.remaining()) {
		return nil, fmt.Errorf("store: value claims %d bytes with %d left: %w", n, r.remaining(), errTruncated)
	}
	out := r.b[r.i : r.i+int(n)]
	r.i += int(n)
	return out, nil
}

// lenPrefixed reads a uvarint length and that many bytes, as a view.
func (r *reader) lenPrefixed() ([]byte, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	return r.next(n)
}

// value reads one tagged value.
func (r *reader) value() (any, error) {
	tag, err := r.readByte()
	if err != nil {
		return nil, err
	}

	switch tag {
	case tagNull:
		return nil, nil

	case tagInt:
		return r.varint()

	case tagUint:
		return r.uvarint()

	case tagFloat32:
		raw, err := r.next(4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(raw)), nil

	case tagFloat64:
		raw, err := r.next(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(raw)), nil

	case tagBool:
		c, err := r.readByte()
		if err != nil {
			return nil, err
		}
		// Anything else did not come from this encoder, and a bool is one bit of
		// meaning; there is no reading of 0x02 worth guessing at.
		if c > 1 {
			return nil, fmt.Errorf("store: boolean byte is 0x%02x, want 0 or 1", c)
		}
		return c == 1, nil

	case tagString:
		s, err := r.lenPrefixed()
		if err != nil {
			return nil, err
		}
		return string(s), nil

	// The copy matters: the buffer handed to DecodeRow belongs to the database
	// driver, which is free to reuse it once the row is stepped past. A []byte
	// value pointing into it would change under the engine between being read and
	// being rendered.
	case tagBytes:
		v, err := r.lenPrefixed()
		if err != nil {
			return nil, err
		}
		out := make([]byte, len(v))
		copy(out, v)
		return out, nil

	case tagTime:
		sec, err := r.varint()
		if err != nil {
			return nil, err
		}
		nsec, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		if nsec > 999999999 {
			return nil, fmt.Errorf("store: timestamp carries %d nanoseconds, want under a second", nsec)
		}
		// Normalized to UTC so a decoded row does not depend on the zone of the
		// machine that happens to read it back.
		return time.Unix(sec, int64(nsec)).UTC(), nil

	// Re-checked rather than trusted. The bytes have been sitting in a file that
	// anything on the host could have edited, and this is the one value that
	// reaches a generated script unquoted — a store that let arbitrary text back
	// out through it would put that text straight into SQL an operator runs.
	case tagRaw:
		v, err := r.lenPrefixed()
		if err != nil {
			return nil, err
		}
		s := string(v)
		if !change.ValidRaw(s) {
			return nil, fmt.Errorf("store: stored raw value %q is not a number; refusing to hand back a value that would be emitted unquoted", s)
		}
		return change.Raw(s), nil
	}

	// Skipping the value would shift every value after it onto the wrong column,
	// and returning nil would write NULL over data that was never NULL.
	return nil, fmt.Errorf("store: unknown value tag 0x%02x", tag)
}
