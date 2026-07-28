package binlog

import (
	"fmt"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
)

// DecodeOptions is how binlog values are turned into Go values.
//
// It exists as one exported type because the two ways into this package — a
// file scan and a replica stream — are configured through different structs
// whose zero values disagree with each other. A parser built by hand and a
// syncer config built by hand would decode the same column differently, and the
// difference is invisible: a TIMESTAMP decoded as a string is formatted in the
// reading machine's own zone, renders without complaint under the script's
// SET time_zone='+00:00', and restores a value shifted by that machine's offset.
//
// Both entry points take these options so that cannot drift.
type DecodeOptions struct {
	// ParseTime decodes temporal columns into time.Time rather than strings
	// already formatted by the library, so the engine controls literal syntax.
	ParseTime bool

	// UseDecimal keeps DECIMAL exact. Decoded as a float it rounds, and a
	// reversal that restores a rounded amount is worse than one that refuses.
	UseDecimal bool

	// VerifyChecksum rejects events out of a truncated or corrupted log, which
	// cannot be trusted to reverse anything.
	VerifyChecksum bool

	// TimestampLocation is the zone used when a timestamp is rendered as a
	// string. It only applies when ParseTime is false, but it is part of the
	// contract either way, so a store recorded under one setting is not read
	// under another.
	TimestampLocation *time.Location
}

// DefaultDecodeOptions is the decoding contract for every source.
func DefaultDecodeOptions() DecodeOptions {
	return DecodeOptions{
		ParseTime:         true,
		UseDecimal:        true,
		VerifyChecksum:    true,
		TimestampLocation: time.UTC,
	}
}

// Fingerprint identifies the options a set of rows was decoded under.
//
// The store records it so rows captured under one contract are never read back
// under another — the values would still decode cleanly and mean something
// different.
func (o DecodeOptions) Fingerprint() string {
	zone := "UTC"
	if o.TimestampLocation != nil {
		zone = o.TimestampLocation.String()
	}
	return fmt.Sprintf("v1;parse_time=%t;use_decimal=%t;verify_checksum=%t;tz=%s",
		o.ParseTime, o.UseDecimal, o.VerifyChecksum, zone)
}

// ApplyToSyncer configures a replica stream to decode by these options.
func (o DecodeOptions) ApplyToSyncer(cfg *replication.BinlogSyncerConfig) {
	cfg.ParseTime = o.ParseTime
	cfg.UseDecimal = o.UseDecimal
	cfg.VerifyChecksum = o.VerifyChecksum
	cfg.TimestampStringLocation = o.location()
}

// applyToParser configures a file parser to decode by these options.
func (o DecodeOptions) applyToParser(p *replication.BinlogParser) {
	p.SetParseTime(o.ParseTime)
	p.SetUseDecimal(o.UseDecimal)
	p.SetVerifyChecksum(o.VerifyChecksum)
	p.SetTimestampStringLocation(o.location())
}

func (o DecodeOptions) location() *time.Location {
	if o.TimestampLocation == nil {
		return time.UTC
	}
	return o.TimestampLocation
}
