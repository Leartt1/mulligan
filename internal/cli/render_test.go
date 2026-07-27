package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
)

func reversal(op change.Op, table string, pos uint32, stmt string) reverse.Reversal {
	return reverse.Reversal{
		Event: change.Event{
			Schema:  "shop",
			Table:   table,
			Op:      op,
			LogFile: "binlog.000004",
			LogPos:  pos,
			At:      time.Date(2026, 7, 27, 12, 3, 11, 0, time.UTC),
		},
		Statement: stmt,
	}
}

func TestRenderWritesEachStatementUnderItsProvenance(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Update, "orders", 4242, "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"UPDATE shop.orders",         // which logged statement is being undone
		"binlog.000004:4242",         // where to find it in the log
		"2026-07-27 12:03:11 UTC",    // when it ran
		"UPDATE `shop`.`orders` SET", // the reversal itself
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// The whole safety model is that a human reads this before running it, so the
// script has to say plainly that nothing has happened yet.
func TestRenderWarnsThatNothingHasBeenExecuted(t *testing.T) {
	plan := []reverse.Reversal{reversal(change.Delete, "orders", 100, "INSERT INTO `shop`.`orders` (`id`) VALUES (7);")}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	if !strings.Contains(out.String(), "REVIEW BEFORE RUNNING") {
		t.Errorf("output carries no review warning:\n%s", out.String())
	}
}

// Timestamps are rendered as UTC and text as utf8mb4, so the script has to put
// the session into those terms before running anything. Without it a session in
// another zone stores TIMESTAMP values shifted by its offset, and one in another
// charset converts the text on the way in — both silently.
func TestRenderPinsSessionTimeZoneAndCharsetBeforeAnyStatement(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Update, "orders", 4242, "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	for _, want := range []string{"SET time_zone = '+00:00';", "SET NAMES utf8mb4;"} {
		at := strings.Index(got, want)
		if at == -1 {
			t.Errorf("script does not pin the session with %q:\n%s", want, got)
			continue
		}
		if stmt := strings.Index(got, "UPDATE `shop`"); at > stmt {
			t.Errorf("%q comes after the first statement, too late to apply:\n%s", want, got)
		}
	}
}

// An empty result is a real answer — the filter matched nothing — and must not
// look like a script that happens to do nothing.
func TestRenderSaysSoWhenNothingMatched(t *testing.T) {
	var out strings.Builder
	if err := Render(&out, "binlog.000004", nil); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "no matching changes") {
		t.Errorf("output does not report an empty result:\n%s", got)
	}
	if strings.Contains(got, "REVIEW BEFORE RUNNING") {
		t.Errorf("empty result carries a review warning for a script with no statements:\n%s", got)
	}
}

func TestRenderCountsTheStatements(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Insert, "orders", 100, "DELETE FROM `shop`.`orders` WHERE `id` = 1 LIMIT 1;"),
		reversal(change.Insert, "orders", 200, "DELETE FROM `shop`.`orders` WHERE `id` = 2 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	if !strings.Contains(out.String(), "2 statements") {
		t.Errorf("output does not state the statement count:\n%s", out.String())
	}
}

// Order carries meaning — reversals run newest first — so rendering must not
// reshuffle what the engine ordered.
func TestRenderPreservesPlanOrder(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Insert, "orders", 200, "DELETE FROM `shop`.`orders` WHERE `id` = 2 LIMIT 1;"),
		reversal(change.Insert, "orders", 100, "DELETE FROM `shop`.`orders` WHERE `id` = 1 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	first := strings.Index(got, "`id` = 2")
	second := strings.Index(got, "`id` = 1")
	if first == -1 || second == -1 {
		t.Fatalf("output missing one of the statements:\n%s", got)
	}
	if first > second {
		t.Errorf("statements were reordered:\n%s", got)
	}
}
