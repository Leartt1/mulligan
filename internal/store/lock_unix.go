//go:build unix

package store

import (
	"fmt"
	"os"
	"syscall"
)

// lock takes an exclusive advisory lock beside the store.
//
// flock rather than a row in the database, because the kernel releases it when
// the process ends however it ends. A row recording an owner and a heartbeat
// would have to guess how long a heartbeat may lapse before the owner is assumed
// dead, and that guess is wrong in both directions: too short and a busy
// collector loses its own store, too long and a restart after a crash sits and
// waits for a lock nobody holds.
func lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: opening the lock beside %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: another collector is already following %s; "+
			"two writing to one store would interleave their changes into an order that means nothing", path)
	}
	return f, nil
}

func unlock(f *os.File) error {
	if f == nil {
		return nil
	}
	// Closing releases the lock; removing the file would race another process that
	// has already opened it and is waiting.
	return f.Close()
}
