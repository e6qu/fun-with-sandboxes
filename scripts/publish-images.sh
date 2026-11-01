#!/usr/bin/env bash
# Push the archives build-images.sh wrote in $1 to registry prefix $2, as
# <prefix>/fws-<name>:<tag> (fws/postgres:16 -> <prefix>/fws-postgres:16).
# Log in first: crane auth login <registry> -u <user> --password-stdin.
set -euo pipefail

dir=${1:?usage: publish-images.sh IMAGE_DIR REGISTRY_PREFIX}
prefix=${2:?usage: publish-images.sh IMAGE_DIR REGISTRY_PREFIX}
crane=(go run github.com/google/go-containerregistry/cmd/crane@v0.22.1)

for archive in "$dir"/*.tar; do
  tag=$(tar -xOf "$archive" manifest.json | sed -E 's/.*"RepoTags":\["([^"]+)".*/\1/')
  case "$tag" in
    fws/*) ;;
    *) echo "$archive: unexpected tag $tag" >&2; exit 1 ;;
  esac
  ref="$prefix/fws-${tag#fws/}"
  echo "pushing $tag as $ref"
  # Push the archive's OCI layout, so the registry gets its manifest byte for
  # byte rather than one rebuilt from manifest.json.
  layout=$(mktemp -d)
  tar -xf "$archive" -C "$layout"
  "${crane[@]}" push "$layout" "$ref"
  rm -rf "$layout"
done
