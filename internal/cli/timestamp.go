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

// parseTimestamp reads a timestamp in one of the forms an operator types.
func parseTimestamp(s string) (time.Time, error) {
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
	return time.Time{}, fmt.Errorf("cannot read %q as a timestamp; try 2006-01-02 15:04:05 or 2006-01-02T15:04:05Z07:00", s)
}
