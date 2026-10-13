#!/bin/sh
# VyOS に持ち込むためのイメージ tar を作る
#   ./scripts/build-image.sh                    -> pingcord-amd64.tar
#   PLATFORM=linux/arm64 ./scripts/build-image.sh -> pingcord-arm64.tar
set -eu
cd "$(dirname "$0")/.."

PLATFORM=${PLATFORM:-linux/amd64}
VERSION=${VERSION:-$(date +%Y%m%d-%H%M)}
ARCH=${PLATFORM#linux/}
OUT=pingcord-$ARCH.tar

docker build --platform "$PLATFORM" --build-arg VERSION="$VERSION" \
  -t localhost/pingcord:latest -t "localhost/pingcord:$VERSION" .
docker save -o "$OUT" localhost/pingcord:latest "localhost/pingcord:$VERSION"
echo "wrote $OUT (version $VERSION)"
