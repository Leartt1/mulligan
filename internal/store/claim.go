package store

import "errors"

// errLockUnsupported means this platform offers no advisory lock, so a second
// collector cannot be detected.
var errLockUnsupported = errors.New("advisory locking is not available on this platform")

// Claim takes exclusive ownership of the store for a collector.
//
// Two collectors writing one store interleave their transactions into a single
// store-assigned order that reflects neither source. The result is not a
// corrupted file — it opens and reads perfectly well — but a revert generated
// from it applies changes in an order that never happened, which is the shape of
// wrong this project spends most of its effort avoiding.
//
// Release returns it. A collector that dies without releasing loses the lock
// anyway, because the kernel drops it when the process ends.
func (s *Store) Claim() error {
	f, err := lock(s.path + ".lock")
	switch {
	case errors.Is(err, errLockUnsupported):
		s.lockUnsupported = true
		return nil
	case err != nil:
		return err
	}

	s.lockFile = f
	return nil
}

// LockUnsupported reports whether ownership could not actually be enforced, so a
// caller can say so rather than implying a guarantee it does not have.
func (s *Store) LockUnsupported() bool { return s.lockUnsupported }

// Release gives up ownership.
func (s *Store) Release() error {
	f := s.lockFile
	s.lockFile = nil
	return unlock(f)
}
