package cli

import (
	"fmt"
	"time"
)

// zonedLayouts carry their own offset and are read exactly as written.
var zonedLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05Z0700",
}

// zonelessLayouts are read in the local zone: a timestamp typed without an
// offset means local time to the person typing it, and reading it as UTC would
// shift the window by the machine's offset and quietly miss the statement they
// are hunting for.
var zonelessLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
}

// timeOnlyLayouts carry no date. An operator reading a time off a monitoring
// graph mid-incident types one of these, and refusing it in favour of a full
// date is friction at the worst possible moment.
var timeOnlyLayouts = []string{
	"15:04:05",
	"15:04",
}

// parseTimestamp reads a timestamp in one of the forms an operator types.
func parseTimestamp(s string) (time.Time, error) {
	return parseTimestampAt(s, time.Now())
}

// parseTimestampAt resolves s against a given moment, so the meaning of a
// date-less time is testable.
//
// A bare time resolves to its most recent occurrence: today if it has already
// passed, otherwise yesterday. Someone looking at an incident at 00:10 and
// typing 23:50 means fifty minutes ago; read as today it would name a moment
// almost a day away, which the store would refuse as a window it has not
// reached. A timestamp carrying a date is never second-guessed, however far
// ahead it sits.
func parseTimestampAt(s string, now time.Time) (time.Time, error) {
	for _, layout := range zonedLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	for _, layout := range zonelessLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}

	for _, layout := range timeOnlyLayouts {
		t, err := time.ParseInLocation(layout, s, now.Location())
		if err != nil {
			continue
		}

		at := time.Date(now.Year(), now.Month(), now.Day(),
			t.Hour(), t.Minute(), t.Second(), 0, now.Location())
		if at.After(now) {
			at = at.AddDate(0, 0, -1)
		}
		return at, nil
	}

	return time.Time{}, fmt.Errorf("cannot read %q as a timestamp; try 15:04, "+
		"2006-01-02 15:04:05, or 2006-01-02T15:04:05Z07:00", s)
}
