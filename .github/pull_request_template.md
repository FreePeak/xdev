## What changed

<!-- One paragraph. What is the behavior now, from the user's side? -->

## Why

<!-- Constraints that shaped it, alternatives rejected, and what you measured.
     Link the issue: `closes #123`, `part of #45`. -->

## How it was verified

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...`
- [ ] `go test -count=1 ./...` (unfiltered — a piped `go test | grep` reports the
      *pipeline's* exit status, which is how two red commits reached main here)
- [ ] `go test -race -count=1 ./...`
- [ ] cross-compiles for all six platforms (`GOOS/GOARCH` × linux/darwin/windows)
      — platform syscalls need a build tag, not a runtime `GOOS` check
- [ ] the specific scenario in the linked issue no longer happens, and the
      reproduction is in the tests or pasted below

**Evidence**

```
paste the run, not a claim
```

## Scope discipline

- [ ] one behavioral change in this PR
- [ ] no new dependency, or the reason is in the "why" above
- [ ] every caller of a changed symbol migrated; no alias, shim, or deprecated
      path left behind
- [ ] no task-tracker file added (`TASKS.md`/`TODO.md` are not how work is
      tracked here — issues are)
- [ ] `docs/PRD.md` updated: the affected milestone/status row and the
      `*Last updated:*` footer, in this PR

## Notes for the reviewer

<!-- Known ceilings on purpose: a global lock, an O(n²) scan, a naive heuristic.
     Say what the limit is and what the upgrade path is, so it is a decision and
     not a surprise. -->
