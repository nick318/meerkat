#!/bin/sh
# `scripts/dev.sh [postgres://...]` builds, signs and launches the app.
#
# Everything goes through scripts/run-signed.sh, so the binary carries a
# stable code signature and the keychain stops asking after every rebuild.
# A plain `go run ./cmd/meerkat` works too, unsigned.
#
# mygo needs the Go toolchain go.mod names and forbids cgo.

set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
exec env CGO_ENABLED=0 GOTOOLCHAIN="${GOTOOLCHAIN_MEERKAT:-go1.27.3}" \
    go run -exec "$root/scripts/run-signed.sh" ./cmd/meerkat "$@"
