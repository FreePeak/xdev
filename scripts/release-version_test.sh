#!/usr/bin/env bash
#
# release-version_test.sh — prove the release-version rule with fixture
# repositories.
#
# Each case builds a tiny git repo in a temp directory, commits a subject, tags
# it, and asserts what scripts/release-version.sh computes. Nothing here
# touches the network or the real repository, so it runs anywhere git does:
#
#   bash scripts/release-version_test.sh
#
# The release pipeline runs this before it publishes a version: the rule that
# ships a release is the rule that was tested.

set -uo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/release-version.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cases=0
fails=0

ok() {
	cases=$((cases + 1))
	echo "ok   - $1"
}

bad() {
	cases=$((cases + 1))
	fails=$((fails + 1))
	echo "FAIL - $1: want [$2], got [$3]" >&2
}

check() { # check <name> <want> <got>
	if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "$2" "$3"; fi
}

# new_repo creates a repository with one seed commit and prints its path.
new_repo() {
	local d="$TMP/$1"
	mkdir -p "$d"
	git -C "$d" init -q >/dev/null 2>&1
	git -C "$d" symbolic-ref HEAD refs/heads/main >/dev/null 2>&1
	git -C "$d" config user.email "test@example.test"
	git -C "$d" config user.name "Release Version Test"
	git -C "$d" config commit.gpgsign false
	git -C "$d" config tag.gpgsign false
	echo "seed" >"$d/file.txt"
	git -C "$d" add -A
	git -C "$d" commit -q -m "chore: seed"
	printf '%s' "$d"
}

# commit adds a commit with the given subject (and optional body).
commit() { # commit <repo> <subject> [body]
	local d="$1"
	local subject="$2"
	local body="${3:-}"
	local args=(-q -m "$subject")
	[ -n "$body" ] && args+=(-m "$body")
	echo "$cases-$fails" >>"$d/file.txt"
	git -C "$d" add -A
	git -C "$d" commit "${args[@]}"
}

# plan runs the script and prints "<version> <bump> <prerelease>".
plan() { # plan <repo> [script args...]
	local d="$1"
	shift
	"$SCRIPT" --repo "$d" "$@" 2>"$TMP/stderr" |
		awk -F= '
			/^version=/    { v = $2 }
			/^bump=/       { b = $2 }
			/^prerelease=/ { p = $2 }
			END           { print v " " b " " p }
		'
}

# --- a repository with no release yet --------------------------------------

r="$(new_repo tagless-feat)"
commit "$r" "feat(tui): a tree, not a flat list"
check "tagless repo with a feature opens at v0.1.0" "v0.1.0 minor false" "$(plan "$r")"

r="$(new_repo tagless-fix)"
commit "$r" "fix(tool): the shell's own words are not commands"
check "tagless repo with a fix opens at v0.1.0" "v0.1.0 patch false" "$(plan "$r")"

# --- the bump after a release ------------------------------------------------

r="$(new_repo after-fix)"
git -C "$r" tag v0.4.368
commit "$r" "fix(config): a setting that is parsed is not a setting that is wired"
check "a fix moves Z" "v0.4.369 patch false" "$(plan "$r")"

r="$(new_repo after-feat)"
git -C "$r" tag v0.4.368
commit "$r" "feat(tui): the subagent strip is a TREE, not a flat list"
check "a feature moves Y and resets Z" "v0.5.0 minor false" "$(plan "$r")"

r="$(new_repo after-enhance)"
git -C "$r" tag v0.4.368
commit "$r" "enhance(tui): the row cap must bound the render work"
check "an enhance moves Y" "v0.5.0 minor false" "$(plan "$r")"

r="$(new_repo after-refactor)"
git -C "$r" tag v0.4.368
commit "$r" "refactor(tui): one dock, two painters"
check "a refactor moves Y" "v0.5.0 minor false" "$(plan "$r")"

r="$(new_repo after-perf)"
git -C "$r" tag v0.4.368
commit "$r" "perf(tui): the row cap must bound the render work"
check "a perf moves Y" "v0.5.0 minor false" "$(plan "$r")"

r="$(new_repo after-docs)"
git -C "$r" tag v0.4.368
commit "$r" "docs(prd): the milestone table is current"
check "a docs-only merge moves Z" "v0.4.369 patch false" "$(plan "$r")"

r="$(new_repo after-mixed)"
git -C "$r" tag v0.4.368
commit "$r" "fix(tool): a loop's scaffolding is not a command"
commit "$r" "feat(agent): the run stops when the answer is in hand"
check "the highest impact in the range wins" "v0.5.0 minor false" "$(plan "$r")"

r="$(new_repo after-untyped)"
git -C "$r" tag v0.4.368
commit "$r" "Keep TUI prompts running after mid-task yields"
check "a subject without a type moves Z" "v0.4.369 patch false" "$(plan "$r")"

# The commits that land on main are squash merges, so their subject carries
# the PR number — the type is still the type.
r="$(new_repo after-merged-fix)"
git -C "$r" tag v0.4.368
commit "$r" "fix(config): a setting that is parsed is not a setting that is wired (#730)"
check "a squash-merged fix moves Z" "v0.4.369 patch false" "$(plan "$r")"

r="$(new_repo after-merged-feat)"
git -C "$r" tag v0.4.368
commit "$r" "feat(tui): the subagent strip is a TREE, not a flat list (#723)"
check "a squash-merged feature moves Y" "v0.5.0 minor false" "$(plan "$r")"

r="$(new_repo after-merge)"
git -C "$r" tag v0.4.368
commit "$r" "feat(agent): the artifact-offload backend ships"
git -C "$r" checkout -q -b side
commit "$r" "fix(tui): drop the transcript-trim notice"
git -C "$r" checkout -q main
git -C "$r" merge -q --no-ff -m "Merge remote-tracking branch 'origin/main'" side >/dev/null
check "a merge commit is not a feature" "v0.5.0 minor false" "$(plan "$r")"

# --- the versions a human pins ------------------------------------------------

r="$(new_repo pinned)"
commit "$r" "feat(tui): a tree, not a flat list"
check "a pinned version wins" "v1.2.3 none false" "$(plan "$r" --tag v1.2.3)"
check "a pinned version without the v prefix is normalized" "v1.2.3 none false" "$(plan "$r" --tag 1.2.3)"
check "a pinned pre-release is a pre-release" "v1.0.0-rc.1 none true" "$(plan "$r" --tag v1.0.0-rc.1)"

if "$SCRIPT" --repo "$r" --tag v1.2 >/dev/null 2>&1; then
	bad "a malformed pinned version is rejected" "exit != 0" "exit 0"
else
	ok "a malformed pinned version is rejected"
fi

r="$(new_repo tagged-ref)"
commit "$r" "feat(tui): a tree, not a flat list"
check "a tag push names its own version" "v9.9.9 none false" \
	"$(plan "$r" --ref refs/tags/v9.9.9 --ref-name v9.9.9)"

# --- a commit that is already released ---------------------------------------

r="$(new_repo already-released)"
git -C "$r" tag v0.4.368
commit "$r" "fix(tool): the shell's own words are not commands"
git -C "$r" tag v0.4.369
check "a tagged HEAD republishes its version" "v0.4.369 none false" "$(plan "$r")"

# --- a breaking change -------------------------------------------------------

r="$(new_repo breaking)"
git -C "$r" tag v0.4.368
commit "$r" "feat(api)!: the endpoint moved"
check "a breaking change still releases" "v0.5.0 minor false" "$(plan "$r")"
if grep -q "::warning::" "$TMP/stderr"; then
	ok "a breaking change is reported"
else
	bad "a breaking change is reported" "::warning::" "$(cat "$TMP/stderr")"
fi

r="$(new_repo breaking-body)"
git -C "$r" tag v0.4.368
commit "$r" "feat(api): the endpoint moved" "BREAKING CHANGE: the endpoint moved"
check "a breaking footer still releases" "v0.5.0 minor false" "$(plan "$r")"
if grep -q "::warning::" "$TMP/stderr"; then
	ok "a breaking footer is reported"
else
	bad "a breaking footer is reported" "::warning::" "$(cat "$TMP/stderr")"
fi

echo
if [ "$fails" -eq 0 ]; then
	echo "release-version: $cases cases, all passed"
	exit 0
fi
echo "release-version: $cases cases, $fails failed" >&2
exit 1
