#!/usr/bin/env bash
#
# release-version.sh — decide the version of the next xdev release.
#
# The maintainer owns the MAJOR version (X): this script never moves it. A
# breaking change is reported as a warning, so the human can cut the major
# release by hand — push a vX.0.0 tag, or dispatch the workflow with --tag.
#
# MINOR (Y) and PATCH (Z) come from the commits that landed since the last
# release tag:
#
#   a feature commit (feat, feature, enhance, refactor, perf, ...)  -> Y+1, Z=0
#   anything else, a bug fix included                               -> Z+1
#
# The range decides by its highest impact: one feature among ten fixes is a
# minor release; ten fixes alone are a patch. A commit whose subject does
# not follow the convention (documented in CONTRIBUTING.md) is not a feature
# either, so it moves Z, never Y — only a declared feature moves Y.
#
# Usage:
#   release-version.sh [--repo DIR] [--ref REF] [--ref-name NAME] [--tag vX.Y.Z]
#
#   --repo DIR      the git repository to read (default: this directory)
#   --ref REF       the ref the run was triggered by; refs/tags/vX.Y.Z names
#                   its own version
#   --ref-name NAME the short name of that ref
#   --tag vX.Y.Z    pin the version (what workflow_dispatch's version input does)
#
# Writes key=value lines on stdout (GITHUB_OUTPUT-friendly):
#
#   version=v0.5.0
#   prerelease=false
#   bump=minor
#   base=v0.4.368
#
# Everything a human should read goes to stderr, so stdout stays parseable.

set -euo pipefail

REPO="."
PINNED=""
REF="${GITHUB_REF:-}"
REF_NAME="${GITHUB_REF_NAME:-}"

usage() {
	cat >&2 <<'EOF'
usage: release-version.sh [--repo DIR] [--ref REF] [--ref-name NAME] [--tag vX.Y.Z]

  --repo DIR       git repository to read (default: the current directory)
  --ref REF        the ref the run was triggered by
  --ref-name NAME  the short name of that ref
  --tag vX.Y.Z     pin the version
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--repo) REPO="${2:?--repo needs a path}"; shift 2 ;;
	--ref) REF="${2:?--ref needs a value}"; shift 2 ;;
	--ref-name) REF_NAME="${2:?--ref-name needs a value}"; shift 2 ;;
	--tag) PINNED="${2:?--tag needs a value}"; shift 2 ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "release-version: unknown argument: $1" >&2
		usage
		exit 2
		;;
	esac
done

cd "$REPO"

note() { echo "$*" >&2; }
warn() { echo "::warning::$*" >&2; }

# is_feature_type reports whether a commit type makes a MINOR release. A bug
# fix is deliberately absent: a fix moves Z, never Y.
is_feature_type() {
	case "$1" in
	feat | feature | enh | enhance | enhancement | refactor | perf | improve | improvement | optimize | optimisation) return 0 ;;
	*) return 1 ;;
	esac
}

# normalize_version turns "1.2.3" into "v1.2.3" and rejects anything that is
# not a semantic version, so a typo in a pinned version fails the plan instead
# of publishing a tag nobody can resolve.
normalize_version() {
	local v="$1"
	case "$v" in
	v*) ;;
	*) v="v$v" ;;
	esac
	if [[ ! "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
		return 1
	fi
	printf '%s' "$v"
}

# first_line returns the first line of a string without a pipe (a pipeline
# into `head` would SIGPIPE its producer and trip `set -o pipefail`).
first_line() {
	printf '%s' "${1%%$'\n'*}"
}

VERSION=""
BUMP=""
BASE=""

# 1. A tag push names its own version — this is how the maintainer cuts a
#    major release by hand.
case "$REF" in
refs/tags/v*) VERSION="$REF_NAME" ;;
esac

# 2. An explicit pin names the version (workflow_dispatch's version input).
if [ -z "$VERSION" ] && [ -n "$PINNED" ]; then
	if ! VERSION="$(normalize_version "$PINNED")"; then
		echo "release-version: \"$PINNED\" is not a semantic version (want vX.Y.Z, optionally with a -suffix)" >&2
		exit 1
	fi
fi

# 3. HEAD already carries a release tag: re-publish that one. The tag is
#    created by `gh release create --target`, so a re-run of the same commit
#    lands here and republishes instead of cutting a second version.
if [ -z "$VERSION" ]; then
	HEAD_TAGS="$(git tag --points-at HEAD --list 'v*')"
	HEAD_TAG="$(first_line "$HEAD_TAGS")"
	if [ -n "$HEAD_TAG" ]; then
		VERSION="$HEAD_TAG"
		note "::notice::HEAD already released as ${HEAD_TAG} — re-publishing it"
	fi
fi

# 4. Compute the version from the commits that landed since the last release.
if [ -z "$VERSION" ]; then
	# The newest version tag wins the ordering (numeric, so v0.10.0 beats
	# v0.9.0). `git describe` is NOT usable here: it matches the nearest
	# *reachable* tag, so a commit that sits between two releases would see
	# the older one.
	TAGS="$(git tag --list 'v[0-9]*' --sort=-v:refname)"
	BASE="$(first_line "$TAGS")"
	NO_BASE=0
	if [ -n "$BASE" ]; then
		RANGE="${BASE}..HEAD"
	else
		# No release yet: every commit counts, and the version opens at
		# v0.1.0 — the version the binary reports before the first stamp.
		BASE="v0.0.0"
		RANGE="HEAD"
		NO_BASE=1
	fi

	MINOR_BUMP=0
	BREAKING=""
	# One awk pass reads every commit in the range and prints four
	# unit-separated fields: the commit type (or "-" when the subject does
	# not follow the convention), the "!" breaking marker, the same marker
	# when the body carries a BREAKING CHANGE footer, and the subject itself.
	# The type is only read when the subject really has one, so "Keep the TUI
	# alive" is not read as a type.
	while IFS=$'\037' read -r type bang breaking subject; do
		[ -n "$type" ] || continue
		if is_feature_type "$type"; then
			MINOR_BUMP=1
		fi
		if [ "$bang" = "!" ] || [ "$breaking" = "1" ]; then
			BREAKING="$subject"
		fi
	done < <(
		git log --format='%s%x1f%b%x1e' "$RANGE" |
			awk -v RS="$(printf '\036')" -v FS="$(printf '\037')" '
				function type_of(subject,   word, rest) {
					if (!match(subject, /^[A-Za-z]+/)) return "-"
					word = substr(subject, RSTART, RLENGTH)
					rest = substr(subject, RLENGTH + 1)
					# The conventional shape: "type(scope)!: subject".
					if (rest !~ /^(\([^)]*\))?(!)?:/) return "-"
					return tolower(word)
				}
				{
					subject = $1
					sub(/^[ \t\r\n]+/, "", subject)
					if (subject == "") next
					# A merge commit carries the merge, not a change of its own.
					if (subject ~ /^Merge / || subject ~ /^merge:/) next
					body = tolower($2)
					bang = ""
					if (subject ~ /^[A-Za-z]+(\([^)]*\))?!:/) bang = "!"
					breaking = ""
					if (body ~ /breaking[ -]change/) breaking = "1"
					print type_of(subject) "\037" bang "\037" breaking "\037" subject
				}
			'
	)

	if [ "$MINOR_BUMP" = "1" ]; then
		BUMP="minor"
	else
		BUMP="patch"
	fi

	CORE="${BASE#v}"
	CORE="${CORE%%-*}"
	MAJOR="${CORE%%.*}"
	REST="${CORE#*.}"
	MINOR="${REST%%.*}"
	PATCH="${REST##*.}"
	case "$BUMP" in
	minor)
		MINOR=$((MINOR + 1))
		PATCH=0
		;;
	patch) PATCH=$((PATCH + 1)) ;;
	esac
	VERSION="v${MAJOR}.${MINOR}.${PATCH}"
	# A repository with no release yet opens at v0.1.0 — the version the
	# binary reports before the first stamp — whatever the commits say.
	if [ "$NO_BASE" = "1" ]; then
		VERSION="v0.1.0"
	fi

	if [ -n "$BREAKING" ]; then
		warn "breaking change in the unreleased range (\"${BREAKING}\") — the major version X is manual: cut it by pushing a vX.0.0 tag, or by dispatching this workflow with an explicit version"
	fi
fi

# A "-suffix" version is a pre-release, which is exactly what
# `xdev update --channel canary` resolves and stable skips.
PRERELEASE=false
case "$VERSION" in
*-*) PRERELEASE=true ;;
esac

printf 'version=%s\n' "$VERSION"
printf 'prerelease=%s\n' "$PRERELEASE"
printf 'bump=%s\n' "${BUMP:-none}"
printf 'base=%s\n' "$BASE"
