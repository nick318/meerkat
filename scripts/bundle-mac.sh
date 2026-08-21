#!/bin/sh
# Build the macOS app bundle for one release channel, sign it, and — when
# notary credentials are in the environment — notarize and staple it.
#
#   scripts/bundle-mac.sh dev
#   scripts/bundle-mac.sh public
#
# The channel is baked into the binary through MEERKAT_CHANNEL (see
# crates/release_channel), and each channel is its own app — its own name
# and its own bundle id — so the two can sit in /Applications side by
# side and neither ever updates onto the other.
#
# Signing: MEERKAT_SIGN_IDENTITY names the certificate, defaulting to the
# local self-signed "Meerkat Dev" one that scripts/run-signed.sh uses. A
# "Developer ID Application" identity gets the hardened runtime and a
# timestamp, which notarization requires; anything else is signed plainly,
# and with no certificate at all the bundle is ad-hoc signed so the script
# still produces something runnable.
#
# Notarization runs only when all three of NOTARY_KEY_PATH (an App Store
# Connect .p8 key), NOTARY_KEY_ID and NOTARY_ISSUER_ID are set.
#
# Outputs, in target/dist:
#   <App Name>.app                    the bundle itself
#   meerkat-<channel>-<arch>.tar.gz   what the updater downloads
#   meerkat-<channel>-<arch>.dmg      what a person downloads
#   *.sha256                          a checksum beside each archive

set -eu

channel="${1:-}"
case "$channel" in
    dev|public) ;;
    *) echo "usage: scripts/bundle-mac.sh dev|public" >&2; exit 1 ;;
esac

root="$(cd "$(dirname "$0")/.." && pwd)"

version="$(sed -n 's/^version = "\(.*\)"$/\1/p' "$root/Cargo.toml" | head -1)"
[ -n "$version" ] || { echo "could not read the version out of Cargo.toml" >&2; exit 1; }

sha="${MEERKAT_COMMIT_SHA:-$(git -C "$root" rev-parse --short=7 HEAD)}"
build="${MEERKAT_BUILD_NUMBER:-$version}"

case "$channel" in
    public)
        app_name="Meerkat"
        bundle_id="com.nick318.meerkat"
        ;;
    dev)
        app_name="Meerkat Dev"
        bundle_id="com.nick318.meerkat-dev"
        ;;
esac

echo "building meerkat $version ($channel, $sha)"
MEERKAT_CHANNEL="$channel" MEERKAT_COMMIT_SHA="$sha" \
    cargo build --release -p meerkat --manifest-path "$root/Cargo.toml"

dist="$root/target/dist"
app="$dist/$app_name.app"
rm -rf "$dist"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

cp "$root/target/release/meerkat" "$app/Contents/MacOS/meerkat"
cp "$root/crates/meerkat/resources/meerkat.icns" "$app/Contents/Resources/meerkat.icns"
sed -e "s/{{APP_NAME}}/$app_name/g" \
    -e "s/{{BUNDLE_ID}}/$bundle_id/g" \
    -e "s/{{VERSION}}/$version/g" \
    -e "s/{{BUILD}}/$build/g" \
    "$root/crates/meerkat/resources/Info.plist.template" \
    > "$app/Contents/Info.plist"
plutil -lint "$app/Contents/Info.plist" >/dev/null

identity="${MEERKAT_SIGN_IDENTITY:-Meerkat Dev}"
if security find-identity -v -p codesigning 2>/dev/null | grep -q "$identity"; then
    case "$identity" in
        *"Developer ID"*)
            echo "signing with \"$identity\" (hardened runtime)"
            codesign --force --options runtime --timestamp \
                --sign "$identity" "$app"
            ;;
        *)
            echo "signing with \"$identity\""
            codesign --force --sign "$identity" "$app"
            ;;
    esac
else
    echo "no \"$identity\" certificate; ad-hoc signing" >&2
    codesign --force --sign - "$app"
fi

if [ -n "${NOTARY_KEY_PATH:-}" ] && [ -n "${NOTARY_KEY_ID:-}" ] && [ -n "${NOTARY_ISSUER_ID:-}" ]; then
    echo "notarizing"
    ditto -c -k --keepParent "$app" "$dist/notarize.zip"
    xcrun notarytool submit "$dist/notarize.zip" \
        --key "$NOTARY_KEY_PATH" \
        --key-id "$NOTARY_KEY_ID" \
        --issuer "$NOTARY_ISSUER_ID" \
        --wait
    rm "$dist/notarize.zip"
    xcrun stapler staple "$app"
fi

case "$(uname -m)" in
    arm64) arch="aarch64" ;;
    x86_64) arch="x86_64" ;;
    *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

archive="$dist/meerkat-$channel-$arch.tar.gz"
tar -C "$dist" -czf "$archive" "$app_name.app"

dmg="$dist/meerkat-$channel-$arch.dmg"
hdiutil create -volname "$app_name" -srcfolder "$app" -ov -format UDZO -quiet "$dmg"

(cd "$dist" && shasum -a 256 "$(basename "$archive")" > "$(basename "$archive").sha256")
(cd "$dist" && shasum -a 256 "$(basename "$dmg")" > "$(basename "$dmg").sha256")

echo "done:"
ls -lh "$dist"
