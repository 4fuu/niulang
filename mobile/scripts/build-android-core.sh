#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
MOBILE_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
CORE_DIR="$MOBILE_DIR/core"
OUTPUT_DIR="$MOBILE_DIR/android/app/libs"
GOMOBILE_VERSION=v0.0.0-20260816165457-f98cc9b3c733
GO_VERSION=$(sed -n 's/^go //p' "$CORE_DIR/go.mod")
export GOTOOLCHAIN="go${GO_VERSION}+auto"

: "${ANDROID_HOME:?ANDROID_HOME must point to the Android SDK}"
if [ -z "${ANDROID_NDK_HOME:-}" ]; then
    ANDROID_NDK_HOME="$ANDROID_HOME/ndk/28.0.12433566"
fi
export ANDROID_NDK_HOME

BUILD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/queqiao-android-core.XXXXXX")
trap 'rm -rf "$BUILD_DIR"' EXIT HUP INT TERM
TOOLS_DIR="$BUILD_DIR/tools"
STAGED_SOURCE="$BUILD_DIR/source"
STAGED_CORE_DIR="$STAGED_SOURCE/mobile/core"
mkdir -p "$TOOLS_DIR" "$OUTPUT_DIR"
ln -s "$MOBILE_DIR/.." "$STAGED_SOURCE"

GOBIN="$TOOLS_DIR" go install "golang.org/x/mobile/cmd/gomobile@$GOMOBILE_VERSION"
GOBIN="$TOOLS_DIR" go install "golang.org/x/mobile/cmd/gobind@$GOMOBILE_VERSION"

cd "$CORE_DIR"
"$SCRIPT_DIR/audit-dependencies.sh"
go test -race ./...
cd "$MOBILE_DIR/.."
go run ./mobile/tools/notices \
    -core "$CORE_DIR" \
    -lock "$MOBILE_DIR/runtime-dependencies.lock" \
    -output "$MOBILE_DIR/legal/THIRD_PARTY_NOTICES.txt"
cd "$STAGED_CORE_DIR"
PATH="$TOOLS_DIR:$PATH" "$TOOLS_DIR/gomobile" bind \
    -trimpath \
    -ldflags="-s -w" \
    -target=android \
    -androidapi=30 \
    -o "$BUILD_DIR/mobilecore.aar" \
    .

"$SCRIPT_DIR/audit-mobile-binary.sh" "$BUILD_DIR/mobilecore.aar"

cp "$BUILD_DIR/mobilecore.aar" "$OUTPUT_DIR/mobilecore.aar"
echo "Wrote $OUTPUT_DIR/mobilecore.aar"
