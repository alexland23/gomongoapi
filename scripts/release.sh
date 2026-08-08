#!/usr/bin/env bash
# Cut a release: tag, push, create the GitHub release from the matching
# CHANGELOG.md section, and close the milestone for that version.
#
# Usage: scripts/release.sh <version>
#   <version> is X.Y.Z, with or without a leading 'v' (e.g. 1.2.0 or v1.2.0).
#
# Requires: git, the gh CLI (authenticated), and a "## [X.Y.Z] - ..." section
# already present in CHANGELOG.md for the version being released.

set -euo pipefail

usage() {
	echo "Usage: $0 <version>" >&2
	echo "  <version> is X.Y.Z, with or without a leading 'v', e.g. 1.2.0" >&2
	exit 1
}

if [[ $# -ne 1 ]]; then
	usage
fi

version="${1#v}"
tag="v${version}"

if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "error: version must look like X.Y.Z (got '$1')" >&2
	exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
	echo "error: gh CLI is required (https://cli.github.com)" >&2
	exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
changelog="$repo_root/CHANGELOG.md"

if [[ ! -f "$changelog" ]]; then
	echo "error: $changelog not found" >&2
	exit 1
fi

if git rev-parse "$tag" >/dev/null 2>&1; then
	echo "error: tag $tag already exists" >&2
	exit 1
fi

# Extract the "## [<version>] - ..." section: everything between that
# heading and the next "## " heading (or end of file), with leading/
# trailing blank lines trimmed.
notes="$(awk -v hdr="## [$version]" '
	index($0, hdr) == 1 { found = 1; next }
	found && index($0, "## ") == 1 { exit }
	found { lines[n++] = $0 }
	END {
		start = 0
		end = n - 1
		while (start < n && lines[start] == "") start++
		while (end >= start && lines[end] == "") end--
		for (i = start; i <= end; i++) print lines[i]
	}
' "$changelog")"

if [[ -z "$notes" ]]; then
	echo "error: no CHANGELOG.md section found for [$version] (expected a heading like '## [$version] - YYYY-MM-DD')" >&2
	exit 1
fi

echo "Release notes for $tag:"
echo "---"
printf '%s\n' "$notes"
echo "---"

echo "Tagging $tag..."
git tag -a "$tag" -m "Release $tag"

echo "Pushing $tag..."
git push origin "$tag"

notes_file="$(mktemp)"
trap 'rm -f "$notes_file"' EXIT
printf '%s\n' "$notes" >"$notes_file"

echo "Creating GitHub release $tag..."
gh release create "$tag" --title "$tag" --notes-file "$notes_file"

# Close the milestone whose title references this version, e.g.
# "M3: Developer Experience (v1.1.0)". Best-effort: warn rather than fail
# the whole release if no matching open milestone is found.
milestone_number="$(gh api "repos/{owner}/{repo}/milestones?state=open" \
	--jq ".[] | select(.title | contains(\"(v${version})\")) | .number" | head -n1)"

if [[ -n "$milestone_number" ]]; then
	echo "Closing milestone #$milestone_number..."
	gh api "repos/{owner}/{repo}/milestones/$milestone_number" -X PATCH -f state=closed >/dev/null
else
	echo "warning: no open milestone found with '(v$version)' in its title; skipping milestone close" >&2
fi

echo "Done: $tag released."
