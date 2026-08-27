#!/usr/bin/env bash

set -euo pipefail

version="${1:-}"
notes_file="docs/releases/${version}.md"

if [[ -z "$version" ]]; then
	echo "usage: $0 vX.Y.Z" >&2
	exit 1
fi

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "version must match vX.Y.Z" >&2
	exit 1
fi

if [[ ! -f "$notes_file" ]]; then
	echo "missing release notes: $notes_file" >&2
	exit 1
fi

if [[ -n "$(git status --porcelain)" ]]; then
	echo "repository changes are not committed" >&2
	exit 1
fi

if git rev-parse "$version" >/dev/null 2>&1; then
	echo "tag $version already exists" >&2
	exit 1
fi

git tag -a "$version" -m "$version"
git push origin HEAD --follow-tags

echo "tagged ${version}; GitHub Actions Release workflow will publish binaries"
