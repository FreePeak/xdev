# Session file format

Reference for xdev's on-disk session store (`internal/session`). The format is
**omp-compatible**: a session written by omp resumes in xdev and vice versa
(pinned by `interop_test.go` against a real omp fixture).

Everything here is derived from the code; when this document and the code
disagree, the code is right — update this file in the same change.

---

## 1. File layout

A session is one JSONL file:

```
line 1   title slot      fixed-width, human-readable, never machine-critical
line 2   session header  {"type":"session","version":3,...}
line 3+  entries         append-only tree, one JSON object per line
```

Location: `<dataDir>/sessions/<cwd-slug>/<RFC3339-timestamp>_<uuid>.jsonl`.
`dataDir` is `~/.xdev/agent` unless `XDEV_*`/config overrides apply.

The header is rewritten in place when the title changes (that is why line 1 is
fixed-width: writing a longer title must not shift every following byte).

## 2. Title slot (line 1)

`MarshalTitleSlot` (`entries.go`) emits a single `titleSlotWire` JSON object
padded to exactly `TitleSlotWidth` bytes including the newline:

| field       | meaning                                              |
|-------------|------------------------------------------------------|
| `type`      | always `"title"`                                     |
| `v`         | slot version (1)                                     |
| `title`     | display title; shrunk one rune at a time to fit      |
| `source`    | `auto` \| `manual` \| `subagent`                     |
| `updatedAt` | RFC3339 UTC                                          |

`source: "subagent"` marks a child session: resume paths (`--continue`,
`--resume`, the picker) skip those so a user never resumes a subagent
transcript. Forks keep `auto` plus a `parentSession` in the header, because a
fork *is* a user session.

Parsers tolerate a missing or malformed slot (`ParseTitleSlotSource` returns
`ok=false`) — the slot is decoration, and a session must remain loadable
without it.

## 3. Session header (line 2)

```json
{"type":"session","version":3,"id":"<uuid>","parentSession":"<uuid>|null",
 "timestamp":"2026-09-11T00:00:00Z","cwd":"/abs/path","title":"…","titleSource":"auto"}
```

`version` is 3. `parentSession` links a fork (or a subagent child) to its
origin for lineage. Unknown fields are preserved on rewrite.

## 4. Entries

Every entry carries a common envelope:

```json
{"type":"<discriminator>","id":"8-hex","parentId":"8-hex|null","timestamp":"<RFC3339 UTC>"}
```

`parentId: null` is the root sentinel (in-memory `""`). The tree is the set of
parent links; the **leaf** is the last entry written, unless a branch marker
re-points it.

| `type`            | payload highlights                                                        |
|-------------------|---------------------------------------------------------------------------|
| `message`         | `message`: the unified `ai.Message` (role `user` \| `assistant` \| `toolResult`, content blocks, usage, model) |
| `model_change`    | `model`: `provider/model` now active                                      |
| `compaction`      | `summary` (a `user` message), `firstKeptEntryId`, `tokensBefore`          |
| `reset_boundary`  | context reset marker: nothing before it is emitted                        |
| `branch_summary`  | summary attached to a branch point                                        |
| `custom`          | `customType` + `data`; xdev persists its branch marker here (`customType: "branch"`, `data.to`) |
| *(unknown)*       | preserved as an opaque chain node — a foreign harness's entry type must not break loading |

### Rendering order (context rebuild)

`BuildContext` walks leaf → root, reverses to chronological order, then:

1. **Reset boundary**: keeps only what follows the *latest* `reset_boundary`.
2. **Compaction**: the *last* compaction in the surviving chain governs —
   its `summary` is emitted first, then the window from `firstKeptEntryId`
   onward (`null` → summary only; an id outside the path → everything after
   the compaction entry).
3. Messages become the model-visible history; the latest `model_change` gives
   the model label; token usage is approximated from the surviving tail.

A missing parent (a dangling link, e.g. an entry skipped as unparseable) ends
the path rather than erroring: partial history beats no session.

## 5. Windowed materialization (memory bound)

`Open` streams the file line by line and **drops entries the next context
build can never read**: everything strictly before the latest
`reset_boundary` / `compaction` window. Indexes are rebuilt in batches
(`loadWindowBatch`), so the cost is amortized rather than per-entry.

Consequences:

- The **file is never modified** — windowing affects only the in-memory view.
- `Store.WindowStats()` reports `retained`, `seen`, and whether windowing
  engaged; the M8 audit test pins a 104 MB transcript at ~62 MB heap.

Because compaction is automatic (M5 threshold behavior), a long session always
carries a boundary, which is what makes the bound reachable in practice.

## 6. Blobs

Large tool outputs are stored out of band, content-addressed:

```
blob:sha256:<64-hex>        reference stored in the entry
<dataDir>/blobs/<hex>       the bytes
```

`BlobStore.Put` is idempotent — identical content maps to one file, so repeated
reads of the same artifact cost one copy on disk.

## 7. Opening, forking, listing

- `Open(path)` — stream + window (above). Never rewrites the file.
- `EnsureOnDisk(path, opts)` — materializes a memory-only session on first
  append: title slot, header, then entries.
- `EnableAutoPersist(path, opts)` — as above, lazily, on the first assistant
  message (a session that never answers leaves no file).
- `ForkSession(src, dst, label)` — copies the file verbatim (title slot,
  header, entries), then rewrites the header with the new id +
  `parentSession`; the child starts as a branch of the same tree. Blocking
  message blobs are shared by reference.
- `List(dataDir)` — reads only the first 4 KiB of each file for the slot +
  header, skipping subagent sessions.

## 8. Compatibility rules

Any change to this format must keep these true:

1. **Unknown entry types are preserved**, not dropped. A session touched by a
   newer harness must survive a round trip through xdev.
2. **The title slot stays fixed-width** and remains optional to parse.
3. **Appends stay atomic per line** (one `Write` of a complete line) so a
   crash truncates at most the last entry.
4. **omp interop**: `interop_test.go` resumes a real omp-generated session —
   that test is the contract, not this prose.
