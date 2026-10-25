#!/usr/bin/env bash
#
# Print the CHANGELOG.md section for one release, to be used as its notes.
#
# Usage: scripts/release-notes.sh v1.2.3
#
# The section is everything between the heading "## [1.2.3]" (a date may
# follow it) and the next "## [" heading. A missing or empty section is an
# error, so a release cannot go out without notes written for it.
set -euo pipefail

TAG="${1:?usage: release-notes.sh <tag, e.g. v1.2.3>}"
VERSION="${TAG#v}"
CHANGELOG="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/CHANGELOG.md"

notes="$(awk -v heading="## [${VERSION}]" '
  index($0, heading) == 1 { found = 1; next }
  found && /^## \[/       { exit }
  found && !started && /^[[:space:]]*$/ { next }
  found                   { started = 1; print }
' "${CHANGELOG}")"

if [[ -z "${notes//[[:space:]]/}" ]]; then
  echo "CHANGELOG.md has no notes for ${VERSION}. Add them under the heading \"## [${VERSION}]\"." >&2
  exit 1
fi

printf '%s\n' "${notes}"
