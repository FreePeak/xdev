package session

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// BlobInfo describes one stored blob: its digest, on-disk size and mtime (the
// GC's retention gate needs the age, and a second stat per blob would race
// with a concurrent Put).
type BlobInfo struct {
	// Digest is the 64-hex sha256 the ref carries.
	Digest  string
	Size    int64
	ModTime time.Time
}

// List enumerates the stored blobs, oldest name first. An absent store is an
// empty list, not an error (a fresh install has nothing to collect).
func (b *BlobStore) List() ([]BlobInfo, error) {
	ents, err := os.ReadDir(b.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: blob list: %w", err)
	}
	out := make([]BlobInfo, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // temp files from Put, and any future dir layout
		}
		info, err := e.Info()
		if err != nil {
			continue // raced with a concurrent Put/Remove
		}
		out = append(out, BlobInfo{Digest: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}
