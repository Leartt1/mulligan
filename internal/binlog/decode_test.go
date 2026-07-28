package binlog

import (
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
)

// The file reader and the replica stream must decode values identically. They
// are configured through different types — a parser and a syncer config — whose
// zero values disagree, so the options have to live in one place that both
// consume.
func TestDecodeOptionsApplyIdenticallyToParserAndSyncer(t *testing.T) {
	opts := DefaultDecodeOptions()

	var cfg replication.BinlogSyncerConfig
	opts.ApplyToSyncer(&cfg)

	if !cfg.ParseTime {
		t.Error("syncer would decode temporal columns as pre-formatted strings, not time.Time")
	}
	if !cfg.UseDecimal {
		t.Error("syncer would decode DECIMAL through float and round it")
	}
	if !cfg.VerifyChecksum {
		t.Error("syncer would accept events from a corrupted log")
	}
	if cfg.TimestampStringLocation != time.UTC {
		t.Errorf("syncer timestamp location = %v, want UTC", cfg.TimestampStringLocation)
	}
}

// A TIMESTAMP decoded as a string is formatted in the reading machine's zone.
// Rendering that under the script's SET time_zone='+00:00' restores a value
// shifted by the collector host's offset, and no test that runs in UTC can see
// it. Both paths must parse into time.Time so the engine controls the syntax.
func TestDefaultDecodeOptionsParseTemporalColumns(t *testing.T) {
	if !DefaultDecodeOptions().ParseTime {
		t.Error("ParseTime is off by default, so temporal columns arrive pre-formatted")
	}
}

func TestDefaultDecodeOptionsKeepDecimalExact(t *testing.T) {
	if !DefaultDecodeOptions().UseDecimal {
		t.Error("UseDecimal is off by default, so DECIMAL rounds through float64")
	}
}

// The store records which options its rows were decoded under. Reading a store
// whose rows were decoded differently would compare values that mean different
// things, so the fingerprint has to change when any option does.
func TestFingerprintChangesWithEveryOption(t *testing.T) {
	base := DefaultDecodeOptions()

	variants := map[string]DecodeOptions{}
	for name, mutate := range map[string]func(o DecodeOptions) DecodeOptions{
		"ParseTime":         func(o DecodeOptions) DecodeOptions { o.ParseTime = !o.ParseTime; return o },
		"UseDecimal":        func(o DecodeOptions) DecodeOptions { o.UseDecimal = !o.UseDecimal; return o },
		"VerifyChecksum":    func(o DecodeOptions) DecodeOptions { o.VerifyChecksum = !o.VerifyChecksum; return o },
		"TimestampLocation": func(o DecodeOptions) DecodeOptions { o.TimestampLocation = time.FixedZone("X", 3600); return o },
	} {
		variants[name] = mutate(base)
	}

	seen := map[string]string{base.Fingerprint(): "default"}
	for name, v := range variants {
		fp := v.Fingerprint()
		if other, clash := seen[fp]; clash {
			t.Errorf("changing %s produced the same fingerprint as %s", name, other)
		}
		seen[fp] = name
	}
}

func TestFingerprintIsStableForTheSameOptions(t *testing.T) {
	if a, b := DefaultDecodeOptions().Fingerprint(), DefaultDecodeOptions().Fingerprint(); a != b {
		t.Errorf("fingerprint is not stable: %q then %q", a, b)
	}
}
