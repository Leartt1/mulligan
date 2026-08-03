// Package api serves what a window store knows over HTTP.
//
// Everything here is read-only. The tool proposes reverts and a human runs
// them, so no route executes SQL against the watched database — a stolen token
// must not be able to change anything, and the console being a viewer is what
// keeps "generate and review" true rather than aspirational.
package api

import (
	"time"

	"github.com/learttytyri/mulligan/internal/store"
)

// Status is the wire shape of a store's health report.
//
// It lives here rather than beside the CLI that first printed it because two
// things publish it — `mulligan status -json` and GET /api/status — and a
// second definition would eventually disagree with the first about what a field
// means. Times are RFC 3339 in UTC and durations are whole seconds, both
// unambiguous to a consumer that is not Go.
type Status struct {
	Store             string          `json:"store"`
	Healthy           bool            `json:"healthy"`
	Verdict           string          `json:"verdict"`
	Source            *StatusSource   `json:"source"`
	Coverage          *StatusCoverage `json:"coverage"`
	StaleSeconds      int64           `json:"stale_seconds"`
	IntegrityProblems []string        `json:"integrity_problems"`
	Gaps              []StatusGap     `json:"gaps"`
	Misses            []StatusMiss    `json:"misses"`
}

// StatusSource is the server a store follows, absent until one is bound.
type StatusSource struct {
	Flavor            string `json:"flavor"`
	ServerIdentity    string `json:"server_identity"`
	GTIDDialect       string `json:"gtid_dialect"`
	DecodeFingerprint string `json:"decode_fingerprint"`
}

// StatusCoverage is the period a store can answer for, absent until it has
// collected something.
type StatusCoverage struct {
	From                string `json:"from"`
	To                  string `json:"to"`
	MaxStalenessSeconds int64  `json:"max_staleness_seconds"`
	RetentionSeconds    int64  `json:"retention_seconds"`
}

// StatusGap is a period the store knows it did not record.
type StatusGap struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// StatusMiss is one change the collector saw and could not store.
type StatusMiss struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// NewStatus renders a store report onto the wire.
//
// Empty collections come out as [] and absent structures as null: a consumer
// iterating gaps should not have to special-case "none", and a store that has
// collected nothing must not report a coverage window beginning in year 1.
func NewStatus(path string, r store.Report) Status {
	out := Status{
		Store:             path,
		Healthy:           r.Healthy(),
		Verdict:           r.Verdict(),
		IntegrityProblems: []string{},
		Gaps:              []StatusGap{},
		Misses:            []StatusMiss{},
	}

	if r.Bound {
		out.Source = &StatusSource{
			Flavor:            r.Binding.Flavor,
			ServerIdentity:    r.Binding.ServerIdentity,
			GTIDDialect:       r.Binding.GTIDDialect,
			DecodeFingerprint: r.Binding.DecodeFingerprint,
		}
	}
	if !r.Coverage.To.IsZero() {
		out.Coverage = &StatusCoverage{
			From:                r.Coverage.From.Format(time.RFC3339),
			To:                  r.Coverage.To.Format(time.RFC3339),
			MaxStalenessSeconds: int64(r.Coverage.MaxStaleness.Seconds()),
			RetentionSeconds:    int64(r.Coverage.Retention.Seconds()),
		}
		out.StaleSeconds = int64(r.Stale.Round(time.Second).Seconds())
	}

	out.IntegrityProblems = append(out.IntegrityProblems, r.Problems...)
	for _, g := range r.Gaps {
		out.Gaps = append(out.Gaps, StatusGap{
			From: g.From.Format(time.RFC3339), To: g.To.Format(time.RFC3339), Reason: g.Reason})
	}
	for _, m := range r.Misses {
		out.Misses = append(out.Misses, StatusMiss{At: m.At.Format(time.RFC3339), Reason: m.Reason})
	}
	return out
}
