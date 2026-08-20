#!/usr/bin/env bash
# Sign and notarize the macOS release binaries.
#
# Signing is deliberately additive. The reproducible tar.gz archives built by
# cmd/queqiaopack are never modified: a Developer ID signature embeds a
# certificate chain and an RFC 3161 timestamp, so a signed binary can never be
# rebuilt byte-for-byte by somebody who does not hold the private key. Breaking
# that would trade the project's verifiable core for Gatekeeper convenience.
#
# Instead each darwin binary is extracted from its reproducible archive, signed,
# notarized, and republished as a separate *_signed.zip asset. Verifiers keep
# using the unsigned archives; users who download through a browser and hit
# Gatekeeper quarantine can use the signed ones.
#
# A bare Mach-O executable cannot carry a stapled notarization ticket; stapling
# is defined for .app, .dmg, .pkg, and .kext only. The ticket is therefore
# published to Apple and resolved online at first launch, which is the normal
# outcome for a notarized command-line tool shipped in an archive.
set -euo pipefail

usage() {
    echo "usage: $0 <dist-dir> <version>" >&2
    exit 2
}

test $# -eq 2 || usage
dist=$1
version=$2
test -d "$dist" || { echo "no such dist directory: $dist" >&2; exit 1; }

: "${APPLE_CERTIFICATE_P12:?base64 Developer ID Application .p12 is required}"
: "${APPLE_CERTIFICATE_PASSWORD:?.p12 export password is required}"
: "${APPLE_SIGNING_IDENTITY:?Developer ID Application identity name is required}"
: "${APPLE_API_KEY_P8:?base64 App Store Connect .p8 key is required}"
: "${APPLE_API_KEY_ID:?App Store Connect key ID is required}"
: "${APPLE_API_ISSUER_ID:?App Store Connect issuer ID is required}"

work=$(mktemp -d)
keychain="$work/queqiao-signing.keychain-db"
keychain_password=$(uuidgen)

cleanup() {
    security delete-keychain "$keychain" 2>/dev/null || true
    rm -rf "$work"
}
trap cleanup EXIT

# An ephemeral keychain keeps the signing key out of the runner's login
# keychain and guarantees it disappears with the job.
security create-keychain -p "$keychain_password" "$keychain"
security set-keychain-settings -lut 900 "$keychain"
security unlock-keychain -p "$keychain_password" "$keychain"

printf '%s' "$APPLE_CERTIFICATE_P12" | base64 --decode > "$work/certificate.p12"
security import "$work/certificate.p12" \
    -k "$keychain" \
    -P "$APPLE_CERTIFICATE_PASSWORD" \
    -T /usr/bin/codesign \
    -x
rm -f "$work/certificate.p12"

# Without this, codesign blocks on an interactive keychain authorization prompt
# that no CI runner can answer.
security set-key-partition-list \
    -S apple-tool:,apple:,codesign: \
    -s -k "$keychain_password" "$keychain" >/dev/null
# Prepend the signing keychain to the search list without dropping the
# existing entries. Paths are read as whole lines, so a keychain path
# containing a space cannot split into two bogus arguments.
existing=()
while IFS= read -r line; do
    line=${line#"${line%%[![:space:]]*}"}
    line=${line%\"}
    line=${line#\"}
    test -n "$line" && existing+=("$line")
done < <(security list-keychains -d user)
security list-keychains -d user -s "$keychain" "${existing[@]}"

security find-identity -v -p codesigning "$keychain" | grep -F "$APPLE_SIGNING_IDENTITY" || {
    echo "identity '$APPLE_SIGNING_IDENTITY' is not present in the imported certificate" >&2
    exit 1
}

printf '%s' "$APPLE_API_KEY_P8" | base64 --decode > "$work/api-key.p8"

signed_dir="$work/signed"
mkdir -p "$signed_dir"

for arch in amd64 arm64; do
    base="queqiaod_${version}_darwin_${arch}"
    archive="$dist/${base}.tar.gz"
    test -f "$archive" || { echo "missing reproducible archive: $archive" >&2; exit 1; }

    stage="$work/stage-$arch"
    mkdir -p "$stage"
    tar -xzf "$archive" -C "$stage"
    binary="$stage/${base}/queqiaod"
    test -f "$binary" || { echo "missing binary in $archive" >&2; exit 1; }

    # Hardened runtime and a secure timestamp are both notarization
    # requirements, not optional hardening.
    codesign --force \
        --sign "$APPLE_SIGNING_IDENTITY" \
        --identifier "me.01.queqiao.queqiaod" \
        --options runtime \
        --timestamp \
        --keychain "$keychain" \
        "$binary"
    codesign --verify --strict --verbose=2 "$binary"

    # ditto writes the archive format notarytool accepts. --keepParent is
    # deliberately omitted: for a single file it stores the enclosing directory
    # name, which would put this job's temporary path inside a published asset
    # and nest the binary a level deeper than the extraction below expects.
    /usr/bin/ditto -c -k "$binary" "$signed_dir/${base}_signed.zip"
done

for arch in amd64 arm64; do
    base="queqiaod_${version}_darwin_${arch}"
    echo "submitting ${base}_signed.zip for notarization"
    xcrun notarytool submit "$signed_dir/${base}_signed.zip" \
        --key "$work/api-key.p8" \
        --key-id "$APPLE_API_KEY_ID" \
        --issuer "$APPLE_API_ISSUER_ID" \
        --wait \
        --timeout 30m

    # notarytool exits non-zero on an Invalid verdict, so reaching here means
    # Apple accepted the submission. Confirm the ticket resolves for the
    # signed binary rather than trusting the submission status alone.
    stage="$work/verify-$arch"
    mkdir -p "$stage"
    /usr/bin/ditto -x -k "$signed_dir/${base}_signed.zip" "$stage"

    # `spctl --type exec` rejects every bare command-line tool with "does not
    # seem to be an app", so it cannot distinguish a notarized binary from an
    # unnotarized one. Assessing the primary signature does: it reports
    # "Notarized Developer ID" only once Apple's ticket resolves for this exact
    # cdhash, and "Unnotarized Developer ID" for a correctly signed binary that
    # was never submitted.
    # spctl exits non-zero on a rejected assessment, so its status is captured
    # rather than allowed to abort the script before the reason is reported.
    spctl --assess --type open --context context:primary-signature -vvv \
        "$stage/queqiaod" > "$work/spctl-$arch.txt" 2>&1 || true
    cat "$work/spctl-$arch.txt"
    grep -qx 'source=Notarized Developer ID' "$work/spctl-$arch.txt" || {
        echo "notarization ticket did not resolve for $base" >&2
        exit 1
    }
done

install -d "$dist/signed"
for arch in amd64 arm64; do
    base="queqiaod_${version}_darwin_${arch}"
    install -m 0644 "$signed_dir/${base}_signed.zip" "$dist/signed/${base}_signed.zip"
done

(cd "$dist/signed" && shasum -a 256 -- *_signed.zip > SHA256SUMS.signed)
echo "signed and notarized macOS assets are in $dist/signed"
