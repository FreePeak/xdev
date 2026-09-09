package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BlobRefPrefix identifies sha256 content-addressed blob references:
// "blob:sha256:<64-hex>" (PRD §3.2).
const BlobRefPrefix = "blob:sha256:"

// BlobStore is a sha256 content-addressed file store under root
// (dataDir/blobs). Put is idempotent: identical content maps to one file.
type BlobStore struct {
	root string
}

// NewBlobStore returns a blob store rooted at dataDir/blobs.
func NewBlobStore(dataDir string) *BlobStore {
	return &BlobStore{root: filepath.Join(dataDir, "blobs")}
}

// Root returns the store's directory.
func (b *BlobStore) Root() string { return b.root }

// Put stores data content-addressed via temp+rename and returns its ref.
func (b *BlobStore) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	ref := BlobRefPrefix + hexSum
	if _, err := os.Stat(b.path(hexSum)); err == nil {
		return ref, nil // already stored
	}
	if err := os.MkdirAll(b.root, 0o755); err != nil {
		return "", fmt.Errorf("session: blob mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(b.root, ".blob-*")
	if err != nil {
		return "", fmt.Errorf("session: blob temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("session: blob write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("session: blob close: %w", err)
	}
	if err := os.Rename(tmpName, b.path(hexSum)); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("session: blob rename: %w", err)
	}
	return ref, nil
}

// Get reads a blob by ref.
func (b *BlobStore) Get(ref string) ([]byte, error) {
	hexSum, err := ParseBlobRef(ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(b.path(hexSum))
	if err != nil {
		return nil, fmt.Errorf("session: blob get %s: %w", ref, err)
	}
	return data, nil
}

// Exists reports whether ref is present in the store.
func (b *BlobStore) Exists(ref string) bool {
	hexSum, err := ParseBlobRef(ref)
	if err != nil {
		return false
	}
	_, err = os.Stat(b.path(hexSum))
	return err == nil
}

func (b *BlobStore) path(hexSum string) string {
	return filepath.Join(b.root, hexSum)
}

// ParseBlobRef validates a "blob:sha256:<hex>" ref and returns the hex
// digest. Hex validation doubles as path-traversal defense.
func ParseBlobRef(ref string) (string, error) {
	hexSum, ok := strings.CutPrefix(ref, BlobRefPrefix)
	if !ok {
		return "", fmt.Errorf("session: invalid blob ref %q", ref)
	}
	if len(hexSum) != sha256.Size*2 {
		return "", fmt.Errorf("session: blob ref digest must be 64 hex chars, got %d", len(hexSum))
	}
	if _, err := hex.DecodeString(hexSum); err != nil {
		return "", fmt.Errorf("session: blob ref digest not hex: %w", err)
	}
	return hexSum, nil
}
