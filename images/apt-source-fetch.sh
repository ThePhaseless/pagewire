#!/bin/sh
# Usage: apt-source-fetch.sh LIST SNAPSHOTS...
#
# Downloads into the working directory (writable by _apt) the Ubuntu source package of each
# line of LIST, a source package name=version (--only-source: never mapped through binary
# names, e.g. coreutils vs coreutils-from). The apt sources need deb-src.
#
# The live archive drops a version once an update supersedes it. Such a line comes from
# snapshot.ubuntu.com (https only, hence ca-certificates) at the times in the SNAPSHOTS files
# (one YYYYMMDDTHHMMSSZ per line, recorded around each apt run of the image), newest first.
# A line no snapshot resolves is named and fails.
# $retry is two words on purpose; snapshot times hold no spaces.
# shellcheck disable=SC2086,SC2013
set -eu
list=$1
shift
retry='-o Acquire::Retries=3'

apt-get $retry update
xargs apt-get $retry source --only-source --download-only < "$list" && exit 0

apt-get install -y --no-install-recommends ca-certificates
missing=$(mktemp)
left=$(mktemp)
while read -r p; do
  apt-get $retry source --only-source --download-only "$p" >&2 || echo "$p"
done < "$list" > "$missing"
for s in $(sort -ru "$@"); do
  [ -s "$missing" ] || break
  apt-get $retry update --snapshot "$s"
  while read -r p; do
    apt-get $retry source --snapshot "$s" --only-source --download-only "$p" >&2 || echo "$p"
  done < "$missing" > "$left"
  mv "$left" "$missing"
done
if [ -s "$missing" ]; then
  echo "in no recorded snapshot:" >&2
  cat "$missing" >&2
  exit 1
fi
