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

// ErrLeaseUntrusted means the lease root cannot host a TRUSTWORTHY lease:
// it is not a private directory, it belongs to another user, it is a
// symlink, or it is not a directory at all.
//
// This is NOT the same failure as "the lease is held" (ErrSessionOwned) and
// NOT the same as "the environment cannot host a lease here" (a read-only
// state root, a directory that cannot be created). A lease taken in an
// untrusted root could be taken by someone else, so a write path REFUSES on
// it: running without the prevention layer is exactly what an adversary
// wants, and a silent downgrade hands it to them.
var ErrLeaseUntrusted = errors.New("session: lease root is not a trustworthy lease host")

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
// private directory this user owns.
//
// The taxonomy matters, because the two failures are handled differently:
//
//   - The lease root is there but is NOT A TRUSTWORTHY LEASE HOST (not
//     private, another user's, a symlink, not a directory) → ErrLeaseUntrusted
//     → the write path REFUSES. A lease taken there could be taken by
//     someone else, and running without the prevention layer is exactly what
//     an adversary wants.
//   - The lease root simply CANNOT EXIST HERE (the parent cannot be created,
//     the state root is read-only) → a plain error → the write path DEGRADES
//     to no lease. That is a misconfiguration, not an attack, and a write
//     path that used to work must keep working.
//
// A symlinked lease root is refused outright rather than resolved: a symlink
// to a private directory PASSES the privacy check, so resolving it would let
// a swapped root redirect the lease onto another inode — two owners of one
// session id, reached through the directory instead of the file.
func ensureLeaseDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			// The path could not even be examined (the parent is not a
			// directory, a permission error): the environment cannot host a
			// lease here.
			return err
		}
		// The lease root does not exist yet: create it, private.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if st, err = os.Lstat(dir); err != nil {
			return err
		}
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink — a lease root that can be redirected is not a lease; fix it by removing the symlink (rm %s) or pointing it at a private directory", ErrLeaseUntrusted, dir, dir)
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: %s exists but is not a directory; fix it by removing it (rm %s)", ErrLeaseUntrusted, dir, dir)
	}
	return checkLeaseDirPrivate(dir, st)
}

// leaseDegraded says out loud that the lease could not be taken because the
// environment cannot host one, and that the write path is carrying on
// without it.
//
// It is LOUD on purpose. The lease is the prevention layer, and a user whose
// lease root is unusable is running without it — silently, forever, unless
// somebody says so. One line per lease root per process: a broken root is
// one fact, not one fact per session.
func leaseDegraded(dir string, err error) {
	leaseWarnMu.Lock()
	defer leaseWarnMu.Unlock()
	if leaseWarned[dir] {
		return
	}
	leaseWarned[dir] = true
	fmt.Fprintf(os.Stderr, "xdev: session lease unavailable (%v) — continuing without it; two xdev processes can write the same session\n", err)
}

// leaseWarned remembers the lease roots this process has already complained
// about.
var (
	leaseWarnMu sync.Mutex
	leaseWarned = map[string]bool{}
)

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
		if errors.Is(err, ErrSessionOwned) {
			_ = s.Close()
			return forkOwnedSession(path, opts, s.ID())
		}
		if errors.Is(err, ErrLeaseUntrusted) {
			// The lease root cannot host a trustworthy lease: a lease taken
			// there could be taken by someone else. REFUSE — running without
			// the prevention layer is what an adversary wants, and a silent
			// downgrade hands it to them.
			_ = s.Close()
			return nil, err
		}
		// The environment cannot host a lease here. The lease is ADVISORY: a
		// lease that cannot be taken must not fail a write path that used to
		// work. The committed-byte boundary still protects the file — and the
		// user is told, because running without the prevention layer must not
		// be silent.
		leaseDegraded(opts.LeaseDir, err)
		return s, nil
	}
	s.lease = l
	return s, nil
}

// forkOwnedSession copies an owned session into a new file beside it and
// takes the lease for the NEW id. The source file is only ever read.
//
// ONE fork per owned session, not one per call: the fork is memoised by the
// SOURCE session id, so a second open-for-write of the same owned session in
// this process reuses the fork instead of littering the session directory with
// a second copy of the same conversation (and the caller with a second tab
// showing it).
func forkOwnedSession(path string, opts Options, srcID string) (*Store, error) {
	if forkPath, ok := memoFork(srcID); ok {
		// The fork this process already made for this session: reuse the FILE
		// (a second handle on it — the in-process refcount shares the lease,
		// exactly as two stores on one session do).
		fork, err := Open(forkPath)
		if err == nil {
			fork.strictFsync = opts.StrictFsync
			if l, lerr := acquireLease(opts.LeaseDir, fork.ID()); lerr == nil && l != nil {
				fork.lease = l
			}
			fork.forkedFrom = path
			fork.forkedFromID = srcID
			return fork, nil
		}
		// The memoised fork is gone (deleted under us): fall through and
		// make a new one.
	}
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
	rememberFork(srcID, forkPath)
	return fork, nil
}

// forks memoises the fork made for an owned session, keyed by the SOURCE
// session id. It is what makes "one fork per owned session" true inside one
// process.
var (
	forkMu sync.Mutex
	forks  = map[string]string{}
)

// memoFork returns the fork this process already made for the source session
// id, if the fork file is still there.
func memoFork(srcID string) (string, bool) {
	forkMu.Lock()
	defer forkMu.Unlock()
	p, ok := forks[srcID]
	if !ok {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		delete(forks, srcID)
		return "", false
	}
	return p, true
}

// rememberFork records the fork made for a source session id.
func rememberFork(srcID, forkPath string) {
	forkMu.Lock()
	defer forkMu.Unlock()
	forks[srcID] = forkPath
}

// takeLeaseLocked takes the ownership lease for this store's session id, if
// one is configured and none is held yet.
//
// A lease that CANNOT be taken because the ENVIRONMENT cannot host one is
// not an error: the lease is advisory, and a write path that used to work
// must not start failing because the lease directory is unusable. The
// committed-byte boundary still protects the file — and the user is told.
//
// A lease root that cannot host a TRUSTWORTHY lease IS an error: the caller
// refuses rather than materialise a session nobody can protect.
func (s *Store) takeLeaseLocked(dir string) error {
	if dir == "" || s.id == "" || s.lease != nil {
		return nil
	}
	l, err := acquireLease(dir, s.id)
	if err != nil {
		if errors.Is(err, ErrLeaseUntrusted) {
			return err
		}
		leaseDegraded(dir, err)
		return nil
	}
	s.lease = l
	return nil
}

// releaseLeaseLocked drops the ownership lease this store holds, if any.
func (s *Store) releaseLeaseLocked() {
	if s.lease == nil {
		return
	}
	s.lease.release()
	s.lease = nil
}

// AttachLease takes the cross-process ownership lease for this store's
// session id and holds it until Close.
//
// It returns ErrSessionOwned when another live process already owns the
// session — the refusal a tool that modifies an existing session file out
// of band (xdev compress) needs: forking there would silently write to a
// file nobody is looking at.
//
// A lease that cannot be TAKEN at all because the ENVIRONMENT cannot host
// one (an unusable lease directory) is not an error: the lease is advisory,
// and a write path that used to work must not start failing because the
// lease directory is unusable. The committed-byte boundary still protects
// the file.
//
// A lease root that cannot host a TRUSTWORTHY lease (ErrLeaseUntrusted) IS
// an error: a lease taken there could be taken by someone else, and the
// write path refuses rather than run without the prevention layer.
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
		if errors.Is(err, ErrSessionOwned) || errors.Is(err, ErrLeaseUntrusted) {
			return err
		}
		leaseDegraded(dir, err)
		return nil // the lease is unavailable — carry on without one
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
