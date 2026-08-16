#!/bin/sh
# Cargo's runner on macOS: give the binary a stable code signature, then
# run it.
#
# Meerkat keeps database passwords in the login keychain, and a keychain
# item's ACL identifies an app by its code signature. Cargo's output is
# ad-hoc signed by the linker, so the identity *is* the hash of the binary
# and changes with every build: macOS reads each rebuild as a different
# app and asks for the keychain password again. "Always Allow" trusts one
# binary, and the next `cargo build` throws that trust away.
#
# Signing with a certificate puts a stable identity in place of the hash,
# so the grant survives a rebuild.
#
# Create the certificate once, in Keychain Access: Certificate Assistant ->
# Create a Certificate, name it as IDENTITY below, type "Code Signing",
# self-signed. Without it this script runs the binary as it found it and
# the prompts carry on, so a machine that has never made one still builds
# and runs.
#
# Cargo hands this script the binary and its arguments. Only the app is
# signed; the test binaries cargo runs through here go straight to exec.

set -eu

IDENTITY="${MEERKAT_SIGN_IDENTITY:-Meerkat Dev}"
binary="$1"
shift

if [ "$(basename "$binary")" = "meerkat" ]; then
    if security find-identity -v -p codesigning 2>/dev/null | grep -q "$IDENTITY"; then
        codesign --force --sign "$IDENTITY" "$binary" >/dev/null 2>&1 ||
            echo "meerkat: \"$IDENTITY\" would not sign the binary; the keychain will keep asking" >&2
    else
        echo "meerkat: no \"$IDENTITY\" signing certificate; the keychain will ask on every rebuild" >&2
    fi
fi

exec "$binary" "$@"
