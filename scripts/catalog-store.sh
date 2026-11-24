#!/usr/bin/env bash
# Keep the catalog's archives with each layer once.
#
# Each archive build-images.sh writes is a whole OCI layout, so the layers
# images share (a closure's store paths: the compiler under fws/node and
# fws/rust, Python under the language images) are in every archive that
# holds them, and the catalog outgrows the Actions cache. A layout's blobs
# are named by their digest, so the same layer is the same file name in
# every archive.
#
#   catalog-store.sh pack ARCHIVES STORE    unpacks each ARCHIVES/*.tar into
#       STORE/<name>, and hard-links every blob to the first file of its
#       digest, so STORE holds each layer once on disk and in any tar that
#       keeps links (actions/cache's; `tar -cf` of STORE). Each archive's
#       .inputs record goes beside its directory.
#   catalog-store.sh unpack STORE ARCHIVES  writes each STORE/<name> back as
#       ARCHIVES/<name>.tar, with its .inputs, as build-images.sh wrote it.
#
# Both leave ARCHIVES or STORE as the only copy they write, replacing what
# was there; neither needs more than the two trees' space.
set -euo pipefail

usage() { echo "usage: catalog-store.sh pack ARCHIVES STORE | unpack STORE ARCHIVES" >&2; exit 2; }
[ $# -eq 3 ] || usage

pack() {
  local archives=$1 store=$2 a name dir blob first
  rm -rf "$store"
  mkdir -p "$store/.pool"
  for a in "$archives"/*.tar; do
    name=$(basename "$a" .tar)
    dir="$store/$name"
    mkdir "$dir"
    tar -xf "$a" -C "$dir"
    if [ -f "$a.inputs" ]; then
      cp "$a.inputs" "$store/$name.inputs"
    fi
    if [ -d "$dir/blobs/sha256" ]; then
      for blob in "$dir"/blobs/sha256/*; do
        first="$store/.pool/$(basename "$blob")"
        if [ -e "$first" ]; then
          ln -f "$first" "$blob"
        else
          ln "$blob" "$first"
        fi
      done
    fi
  done
  # The pool's names are links to blobs the directories hold already.
  rm -rf "$store/.pool"
}

unpack() {
  local store=$1 archives=$2 dir name
  mkdir -p "$archives"
  archives=$(cd "$archives" && pwd)
  for dir in "$store"/*/; do
    dir=${dir%/}
    name=$(basename "$dir")
    # The layout's own entries, not "./": readers of the archive look up
    # index.json and manifest.json by those names.
    (cd "$dir" && tar -cf "$archives/$name.tar.tmp" -- *)
    mv "$archives/$name.tar.tmp" "$archives/$name.tar"
    if [ -f "$store/$name.inputs" ]; then
      cp "$store/$name.inputs" "$archives/$name.tar.inputs"
    fi
  done
}

case $1 in
pack) pack "$2" "$3" ;;
unpack) unpack "$2" "$3" ;;
*) usage ;;
esac
