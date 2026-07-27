package cli

import (
	"testing"
	"time"
)

// An operator reaching for --from types what their logs and their shell history
// already show them, which is rarely RFC 3339 with a zone suffix.
func TestParseTimestampAcceptsTheFormsOperatorsType(t *testing.T) {
	local := func(y int, mo time.Month, d, h, mi, s int) time.Time {
		return time.Date(y, mo, d, h, mi, s, 0, time.Local)
	}

	tests := []struct {
		in   string
		want time.Time
	}{
		{"2026-07-27 12:03:11", local(2026, 7, 27, 12, 3, 11)},
		{"2026-07-27T12:03:11", local(2026, 7, 27, 12, 3, 11)},
		{"2026-07-27", local(2026, 7, 27, 0, 0, 0)},
		{"2026-07-27T12:03:11Z", time.Date(2026, 7, 27, 12, 3, 11, 0, time.UTC)},
		{"2026-07-27T14:03:11+02:00", time.Date(2026, 7, 27, 12, 3, 11, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseTimestamp(tt.in)
			if err != nil {
				t.Fatalf("parseTimestamp(%q) returned error: %v", tt.in, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("parseTimestamp(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// A zoneless timestamp means local time to the person typing it. Reading it as
// UTC would silently shift the window by the machine's offset and quietly miss
// the very statement they are hunting for.
func TestParseTimestampReadsZonelessInputAsLocalTime(t *testing.T) {
	got, err := parseTimestamp("2026-07-27 12:03:11")
	if err != nil {
		t.Fatalf("parseTimestamp returned error: %v", err)
	}

	_, offset := got.Zone()
	_, wantOffset := time.Date(2026, 7, 27, 12, 3, 11, 0, time.Local).Zone()
	if offset != wantOffset {
		t.Errorf("offset = %d, want the local offset %d", offset, wantOffset)
	}
}

func TestParseTimestampRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "yesterday", "27/07/2026", "2026-13-01"} {
		t.Run(in, func(t *testing.T) {
			if got, err := parseTimestamp(in); err == nil {
				t.Errorf("parseTimestamp(%q) = %s, want error", in, got)
			}
		})
	}
}
