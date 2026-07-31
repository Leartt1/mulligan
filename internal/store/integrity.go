package store

import (
	"fmt"
)

// Integrity reports what is wrong with the store file, or nothing if it is
// sound. An error means the check could not run; problems mean it ran and found
// something.
//
// This exists because the store outlives the process writing it and is, once the
// source's binlogs rotate, the only record of the changes it holds. A collector
// killed mid-write, a disk that filled, or a backup that copied the database
// without its write-ahead log all leave a file that opens perfectly well and is
// missing something. An operator needs to be able to ask.
func (s *Store) Integrity() ([]string, error) {
	var problems []string

	// SQLite's own structural check first: if the file is damaged, nothing below
	// this is worth reporting.
	rows, err := s.db.Query(`PRAGMA integrity_check`)
	if err != nil {
		return nil, fmt.Errorf("store: checking %s: %w", s.path, err)
	}
	defer rows.Close()

	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return nil, fmt.Errorf("store: checking %s: %w", s.path, err)
		}
		if result != "ok" {
			problems = append(problems, "the database file is damaged: "+result)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: checking %s: %w", s.path, err)
	}

	// A transaction with no rows is what a half-written append would leave. Appends
	// are atomic, so this should be impossible — which is the reason to check, since
	// the invariant is only worth anything if someone would notice it breaking.
	var rowless int
	err = s.db.QueryRow(
		`SELECT count(*) FROM txn WHERE id NOT IN (SELECT txn_id FROM row_change)`).Scan(&rowless)
	if err != nil {
		return nil, fmt.Errorf("store: checking %s: %w", s.path, err)
	}
	if rowless > 0 {
		problems = append(problems, fmt.Sprintf(
			"%s recorded with no rows, which means an append did not complete", plural(rowless, "transaction")))
	}

	// The checkpoint decides where collection resumes. Ahead of the newest stored
	// change it would resume past changes that were never recorded, leaving a hole
	// with nothing to describe it.
	var ahead int
	err = s.db.QueryRow(
		`SELECT count(*) FROM checkpoint
		  WHERE id = 1
		    AND updated_at > COALESCE((SELECT MAX(committed_at) FROM txn), updated_at)`).Scan(&ahead)
	if err != nil {
		return nil, fmt.Errorf("store: checking %s: %w", s.path, err)
	}
	if ahead > 0 {
		problems = append(problems,
			"the checkpoint is ahead of the newest stored change, so resuming from it would skip changes")
	}

	return problems, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
