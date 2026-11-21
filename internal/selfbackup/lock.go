//go:build unix

package selfbackup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another process (usually a running Docklet) holds the data
// directory.
var ErrLocked = errors.New("the data directory is in use: is Docklet running? Stop it first")

// Lock takes an exclusive advisory lock on <dataDir>/docklet.lock. serve holds
// it for its lifetime; restore takes it so it never swaps files under a live
// server. The lock is released when the returned function is called or the
// process exits.
func Lock(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "docklet.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed name
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
