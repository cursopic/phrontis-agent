#!/usr/bin/env bash
# Release binaries: phrontis for darwin/linux x amd64/arm64 plus dist/SHA256SUMS.
# Usage: scripts/release-build.sh <version>   (e.g. v0.1.0)
# shortcut: the CLI has no version variable, so the version only names the files; add -ldflags -X when one exists.
set -euo pipefail
cd "$(dirname "$0")/.."
version=${1:?usage: scripts/release-build.sh <version>}
rm -rf dist && mkdir dist
for os in darwin linux; do
  for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" \
      -o "dist/phrontis_${version}_${os}_${arch}" ./cmd/phrontis
  done
done
(cd dist && shasum -a 256 phrontis_* > SHA256SUMS)
echo "release-build OK: $(ls dist | wc -l | tr -d ' ') files in dist/"
