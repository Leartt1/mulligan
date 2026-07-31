//go:build !unix

package store

import "os"

// lock is a no-op where advisory locking is not available.
//
// The protection is not silently dropped: Claim reports that it could not be
// taken, and the caller says so. Running two collectors against one store there
// is possible and produces an order that means nothing, so the warning matters.
func lock(path string) (*os.File, error) { return nil, errLockUnsupported }

func unlock(f *os.File) error { return nil }
