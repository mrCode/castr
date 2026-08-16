// Package daemon is the long-running half of castr: it owns discovery, the
// session records, and the backends, and answers clients over a unix socket.
package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockFilename lives in the state directory beside the socket.
const LockFilename = "daemon.lock"

// LockWait covers the window where a daemon is shutting down but has not yet
// dropped its lock. Without it, a client spawning a replacement in that window
// would end up with no daemon at all.
const LockWait = 3 * time.Second

const lockPoll = 100 * time.Millisecond

// ErrLocked means another daemon is already running.
var ErrLocked = errors.New("another castr daemon is already running")

// Lock is a held daemon lock. It must stay alive for as long as the daemon
// runs: the lock belongs to the open file description, so closing the file --
// or the process exiting for any reason, including a crash -- releases it. A
// daemon that dies badly therefore cannot lock out its successors.
type Lock struct {
	file *os.File
}

// Release drops the lock. Safe on a Lock that never took one.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return f.Close()
}

// Acquire takes the daemon lock, waiting up to LockWait for a departing daemon
// to release it, and returns ErrLocked if one is really still there.
//
// Nothing used to enforce one daemon at a time. The server unlinks an existing
// socket and rebinds, so a second daemon simply took over, and a client spawns
// one whenever the socket is momentarily absent -- during another daemon's idle
// shutdown, or when two commands race. Three were observed running at once.
//
// That was not merely untidy. Daemon startup sweeps leftover castr virtual
// outputs, which is right after a crash and catastrophic otherwise: a second
// daemon cannot tell a leftover from the output of a cast running right now in
// a different process. It removed the live one, and the cast died mid-stream
// with nothing in its own daemon's log to explain it -- the removal had
// happened somewhere else entirely.
//
// CALL THIS BEFORE THE SWEEP AND BEFORE TOUCHING THE SOCKET, so a second daemon
// exits before it can do any damage.
func Acquire(path string, wait time.Duration) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return unlocked(err), nil
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return unlocked(err), nil
	}

	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{file: file}, nil
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, ErrLocked
		}
		time.Sleep(lockPoll)
	}
}

// unlocked stands in for a lock we could not create, so callers have one code
// path. It grants nothing and protects nothing: being unable to create a lock
// file must not stop the user casting, and losing single-instance protection is
// the lesser failure.
func unlocked(cause error) *Lock {
	_ = cause // the caller logs; the daemon still starts
	return &Lock{}
}

// LockPath returns the lock file inside a state directory.
func LockPath(stateDir string) string { return filepath.Join(stateDir, LockFilename) }
