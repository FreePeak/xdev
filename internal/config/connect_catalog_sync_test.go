package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The generated catalog must agree with the PINNED snapshot of the effort
// data it was built from.
//
// internal/config/connect_catalog.go is generated (scripts/gen_connect.go
// from a models.dev snapshot) and says DO NOT EDIT — but nothing ran the
// generator against the snapshot, so a hand-edit of the catalog would have
// passed unnoticed. The pinned fixture is that snapshot, distilled to the
// one field the generator reads: the effort levels each model claims.
//
// The generator writes both files together, so a regeneration that changes
// the catalog changes the fixture too — and this test fails until both are
// committed.
func TestTheConnectCatalogMatchesThePinnedEffortSnapshot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "connect_effort.json"))
	if err != nil {
		t.Fatalf("read the pinned effort snapshot: %v", err)
	}
	var pinned map[string][]string
	if err := json.Unmarshal(raw, &pinned); err != nil {
		t.Fatalf("the pinned effort snapshot is not JSON: %v", err)
	}

	// What the committed catalog actually carries.
	got := map[string][]string{}
	for name, e := range connectCatalog {
		for _, m := range e.Models {
			if len(m.Effort) == 0 {
				continue
			}
			got[name+"/"+m.ID] = m.Effort
		}
	}

	var missing, extra, wrong []string
	for key, levels := range pinned {
		have, ok := got[key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		if !sameLevels(have, levels) {
			wrong = append(wrong, key+": catalog "+strings.Join(have, ",")+" != snapshot "+strings.Join(levels, ","))
		}
	}
	for key := range got {
		if _, ok := pinned[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(wrong)
	for _, m := range missing {
		t.Errorf("%s: the snapshot claims an effort the catalog does not carry", m)
	}
	for _, m := range extra {
		t.Errorf("%s: the catalog claims an effort the snapshot does not", m)
	}
	for _, m := range wrong {
		t.Errorf("%s", m)
	}
	if len(pinned) == 0 {
		t.Fatal("the pinned effort snapshot is empty")
	}
}

// The levels a model claims are the vocabulary xdev's config names, and
// nothing else: a level no provider speaks is a request the provider
// rejects.
func TestTheConnectCatalogEffortLevelsAreOnTheLadder(t *testing.T) {
	for name, e := range connectCatalog {
		for _, m := range e.Models {
			for _, lvl := range m.Effort {
				if !IsEffort(lvl) {
					t.Errorf("%s/%s: effort level %q is not on the ladder (%s)", name, m.ID, lvl, strings.Join(EffortLevels, "|"))
				}
			}
		}
	}
}

// sameLevels compares two level lists as sets.
func sameLevels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
