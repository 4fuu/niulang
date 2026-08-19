#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
MOBILE_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
BUILD_LOCK="$MOBILE_DIR/build-tools.lock"

entry=$(sed -n '/^swiftlint@/p' "$BUILD_LOCK")
if [ -z "$entry" ] || [ "$(printf '%s\n' "$entry" | wc -l | tr -d ' ')" -ne 1 ]; then
    echo "build-tools.lock must contain exactly one SwiftLint entry" >&2
    exit 1
fi
IFS='|' read -r component license source digest extra <<EOF
$entry
EOF
if [ "$license" != MIT ] || [ "$source" != https://github.com/realm/SwiftLint ] || [ -n "$extra" ]; then
    echo "invalid SwiftLint build-tool lock entry" >&2
    exit 1
fi
version=${component#swiftlint@}
checksum=${digest#sha256:}
case "$component:$version" in
    swiftlint@*:*.*) ;;
    *)
        echo "invalid SwiftLint version in build-tools.lock" >&2
        exit 1
        ;;
esac
case "$version" in
    *[!0-9.]*|''|.*|*.)
        echo "invalid SwiftLint version in build-tools.lock" >&2
        exit 1
        ;;
esac
case "$digest:$checksum" in
    sha256:*:*) ;;
    *)
        echo "invalid SwiftLint digest in build-tools.lock" >&2
        exit 1
        ;;
esac
case "$checksum" in
    *[!0-9a-f]*|'')
        echo "invalid SwiftLint checksum in build-tools.lock" >&2
        exit 1
        ;;
esac
if [ "${#checksum}" -ne 64 ]; then
    echo "invalid SwiftLint checksum length in build-tools.lock" >&2
    exit 1
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/queqiao-swiftlint.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
archive="$work/portable_swiftlint.zip"
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
    "$source/releases/download/$version/portable_swiftlint.zip" \
    --output "$archive"
printf '%s  %s\n' "$checksum" "$archive" | shasum -a 256 -c -
unzip -q "$archive" -d "$work"
chmod 0755 "$work/swiftlint"
"$work/swiftlint" "$@"
