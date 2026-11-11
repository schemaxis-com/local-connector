#!/usr/bin/env bash
#
# Print the notes of one release: the "## About" section of CHANGELOG.md, which
# opens every release, then the section for the release itself.
#
# Usage: scripts/release-notes.sh v1.2.3
#
# The release's section is everything between the heading "## [1.2.3]" (a date
# may follow it) and the next "## " heading. A missing or empty section, About
# included, is an error, so a release cannot go out without notes written for
# it.
set -euo pipefail

TAG="${1:?usage: release-notes.sh <tag, e.g. v1.2.3>}"
VERSION="${TAG#v}"
CHANGELOG="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/CHANGELOG.md"

# section HEADING prints the lines under HEADING up to the next "## " heading,
# without the blank lines that open it.
section() {
  awk -v heading="$1" '
    index($0, heading) == 1 { found = 1; next }
    found && /^## /         { exit }
    found && !started && /^[[:space:]]*$/ { next }
    found                   { started = 1; print }
  ' "${CHANGELOG}"
}

about="$(section "## About")"
if [[ -z "${about//[[:space:]]/}" ]]; then
  echo "CHANGELOG.md has no About section. Add it under the heading \"## About\", above the releases." >&2
  exit 1
fi

notes="$(section "## [${VERSION}]")"
if [[ -z "${notes//[[:space:]]/}" ]]; then
  echo "CHANGELOG.md has no notes for ${VERSION}. Add them under the heading \"## [${VERSION}]\"." >&2
  exit 1
fi

printf '%s\n\n## In this release\n\n%s\n' "${about}" "${notes}"
