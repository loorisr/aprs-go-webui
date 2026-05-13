#!/bin/sh
set -e

GO=go
SRC=.
OUT=build

rm -rf "$OUT"
mkdir -p "$OUT"

echo "Building..."

for target in \
  "linux/amd64" \
  "linux/386" \
  "windows/amd64" \
  "windows/386"; do

  os="${target%/*}"
  arch="${target#*/}"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"

  echo "  $os/$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" $GO build -o "$OUT/aprs-go-map-${os}-${arch}${ext}" "$SRC"
done

echo "Done — binaries in $OUT/"
ls -lh "$OUT/"
