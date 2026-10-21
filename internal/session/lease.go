package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// The cross-process session ownership lease.
//
// One session may have ONE writer. xdev's committed-byte boundary (#122)
// already detects a second writer — but only AFTER the second writer has
// written, and its refusal arrives as a persistence error mid-turn. The
// lease is the prevention layer in front of it: the process that opens a
// session for writing TAKES the session's lease, and a process that finds
// the lease held forks the session instead of writing into the file.
//
// The lease is ADVISORY. It cannot stop a writer that does not take it (an
// older xdev build, an editor, a script), so the committed-byte boundary
// stays exactly as it is — it remains the authoritative detection layer, and
// a lease holder that finds foreign bytes still gets ErrConcurrentWriter.
//
// The lock primitive is flock(2) on unix and LockFileEx on windows: the
// kernel drops the lock when the holding descriptor closes, including on
// SIGKILL and on a power loss. There is therefore NO stale lease to
// reclaim — a crashed holder needs no cleanup at all, which is the one
// property a PID-file design cannot give.

// ErrSessionOwned means another live process holds the ownership lease for
// this session id.
var ErrSessionOwned = errors.New("session: owned by another process")

// lease is one held ownership lease: an open descriptor on
// <dir>/<sessionId>.lock plus the number of in-process holders sharing it.
//
// The lease file is NEVER unlinked. Unlinking a flock'd file races: the
// holder unlocks and unlinks while another process holds a lock on the now
// unlinked inode, a third process then creates a fresh file and locks it,
// and two owners hold the same session id. The file is 0 bytes and inert —
// its presence means nothing.
type lease struct {
	key     string
	path    string
	file    *os.File
	holders int
}

// leases is the in-process refcount table, keyed by lease dir + session id.
//
// The refcount exists because flock conflicts between two open file
// descriptions of the same file EVEN WITHIN ONE PROCESS: without it the
// TUI's own tab set and a resume of the same session would fork each other.
var (
	leaseMu sync.Mutex
	leases  = map[string]*lease{}
)

// leaseIDPattern is the set of session ids that may name a lease file
// verbatim (omp's sanitiser). xdev's ids are uuid-v4 with dashes, so they
// always match; the fallback exists for a hand-edited header.
var leaseIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// acquireLease takes the ownership lease for the session id in dir.
//
// It returns ErrSessionOwned when another LIVE PROCESS holds it. A second
// holder inside this process shares the lease instead of conflicting.
func acquireLease(dir, id string) (*lease, error) {
	if dir == "" {
		return nil, nil // the lease is disabled (no lease root configured)
	}
	if id == "" {
		return nil, errors.New("session: cannot lease a session with no id")
	}
	key := filepath.Clean(dir) + "\x00" + id

	leaseMu.Lock()
	defer leaseMu.Unlock()
	if l, ok := leases[key]; ok {
		l.holders++
		return l, nil
	}
	if err := ensureLeaseDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, leaseFileName(id))
	f, err := openLeaseFile(path)
	if err != nil {
		return nil, fmt.Errorf("session: open lease %s: %w", path, err)
	}
	if err := lockLeaseFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrSessionOwned) {
			return nil, fmt.Errorf("%w: another xdev is writing this session", ErrSessionOwned)
		}
		return nil, fmt.Errorf("session: lock lease %s: %w", path, err)
	}
	l := &lease{key: key, path: path, file: f, holders: 1}
	leases[key] = l
	return l, nil
}

// release drops one in-process holder. The lock is released and the
// descriptor closed when the LAST holder goes; the lease file stays on disk.
func (l *lease) release() {
	if l == nil {
		return
	}
	leaseMu.Lock()
	defer leaseMu.Unlock()
	l.holders--
	if l.holders > 0 {
		return
	}
	delete(leases, l.key)
	_ = unlockLeaseFile(l.file)
	_ = l.file.Close()
}

// leaseFileName maps a session id to its lease file name. An id that is not
// a plain word is hashed, so a hand-edited header can never escape the
// lease directory.
func leaseFileName(id string) string {
	if leaseIDPattern.MatchString(id) {
		return id + ".lock"
	}
	sum := sha256.Sum256([]byte(id))
	return "h-" + hex.EncodeToString(sum[:])[:16] + ".lock"
}

// ensureLeaseDir creates the lease directory (0700) and verifies it is a
// private directory this user owns. A lease another user could read or
// steal is not a lease, so a directory with group/other bits is refused
// rather than repaired: silently chmod'ing it would hide a misconfigured
// install behind a working lock.
func ensureLeaseDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("session: create lease dir %s: %w", dir, err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("session: stat lease dir %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("session: lease dir %s is not a directory", dir)
	}
	return checkLeaseDirPrivate(dir, st)
}

// OpenForWrite opens path for appending and takes the cross-process
// ownership lease for the session id in the file's header.
//
// If another LIVE PROCESS owns the session, the session is FORKED: the
// transcript is copied to a new file in the same directory under a new id,
// the new header carries parentSession = <old id>, and the fork is opened
// for writing. The original file is left untouched — not written, not
// truncated, not unlinked — for the process that owns it. The returned
// store reports ForkedFrom / ForkReason so the caller can tell the user.
//
// The lease is taken AT OPEN, not lazily at the first write: xdev's own
// precedent (checkSessionLinks, links_unix.go) refuses a hard-linked
// session at open "so the diagnostic names the session the user asked for
// instead of arriving later as a persistence error". The user should
// learn at resume time.
//
// A read-only Open is NEVER refused and never takes a lease: export,
// search, gallery and share only read, and refusing to read because
// someone else writes would be a usability regression with no safety
// benefit. The committed-byte boundary already guarantees a reader never
// sees a torn record.
func OpenForWrite(path string, opts Options) (*Store, error) {
	s, err := Open(path)
	if err != nil {
		return nil, err
	}
	s.strictFsync = opts.StrictFsync
	if opts.LeaseDir == "" || s.ID() == "" {
		// No lease root configured (read-only tools, tests): the session is
		// opened exactly as Open opens it.
		return s, nil
	}
	l, err := acquireLease(opts.LeaseDir, s.ID())
	if err != nil {
		_ = s.Close()
		if !errors.Is(err, ErrSessionOwned) {
			return nil, err
		}
		return forkOwnedSession(path, opts, s.ID())
	}
	s.lease = l
	return s, nil
}

// forkOwnedSession copies an owned session into a new file beside it and
// takes the lease for the NEW id. The source file is only ever read.
func forkOwnedSession(path string, opts Options, srcID string) (*Store, error) {
	newID := NewSessionID()
	now := time.Now().UTC()
	// The canonical session file name (listing.go), placed in the SOURCE's
	// own directory so the fork sits next to the session it branched from.
	forkPath := filepath.Join(filepath.Dir(path),
		now.UTC().Format("2006-01-02T15-04-05.000Z")+"_"+newID+".jsonl")
	fork, err := ForkSession(path, forkPath, "")
	if err != nil {
		return nil, fmt.Errorf("session: fork %s: %w", path, err)
	}
	fork.strictFsync = opts.StrictFsync
	l, err := acquireLease(opts.LeaseDir, fork.ID())
	if err != nil {
		_ = fork.Close()
		return nil, err
	}
	fork.lease = l
	fork.forkedFrom = path
	fork.forkedFromID = srcID
	return fork, nil
}

// AttachLease takes the cross-process ownership lease for this store's
// session id and holds it until Close.
//
// It returns ErrSessionOwned when another live process already owns the
// session — the refusal a tool that modifies an existing session file out
// of band (xdev compress) needs: forking there would silently write to a
// file nobody is looking at.
func (s *Store) AttachLease(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session: lease on a closed store")
	}
	if s.lease != nil {
		return nil // already held
	}
	if dir == "" || s.id == "" {
		return nil
	}
	l, err := acquireLease(dir, s.id)
	if err != nil {
		return err
	}
	s.lease = l
	return nil
}

// ForkedFrom returns the path of the session this store was forked from —
// empty when the store is not a fork.
func (s *Store) ForkedFrom() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forkedFrom
}

// ForkedFromID returns the session id of the session this store was forked
// from — empty when the store is not a fork.
func (s *Store) ForkedFromID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forkedFromID
}

// ForkReason says WHY the store was forked: "owned" — another live
// process held the session's ownership lease. Empty when it was not.
func (s *Store) ForkReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forkedFrom == "" {
		return ""
	}
	return "owned"
}
