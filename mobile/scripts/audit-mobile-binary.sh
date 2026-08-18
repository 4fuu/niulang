#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: audit-mobile-binary.sh AAR_OR_XCFRAMEWORK" >&2
    exit 2
fi

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
MOBILE_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
PROJECT_DIR=$(CDPATH='' cd -- "$MOBILE_DIR/.." && pwd)
LOCK_FILE="$MOBILE_DIR/runtime-dependencies.lock"
GO_VERSION=$(sed -n 's/^go //p' "$MOBILE_DIR/core/go.mod")
ARTIFACT=$1

AUDIT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/queqiao-mobile-binary-audit.XXXXXX")
trap 'rm -rf "$AUDIT_DIR"' EXIT HUP INT TERM

case "$ARTIFACT" in
    *.aar)
        unzip -q "$ARTIFACT" 'jni/arm64-v8a/libgojni.so' -d "$AUDIT_DIR"
        go version -m "$AUDIT_DIR/jni/arm64-v8a/libgojni.so" >"$AUDIT_DIR/buildinfo"
        awk '$1 == "dep" && $2 != "github.com/bojieli/queqiao" && $2 != "github.com/bojieli/queqiao/mobile/core" { print $2 "@" $3 }' \
            "$AUDIT_DIR/buildinfo" | sort -u >"$AUDIT_DIR/actual"
        grep -Eq '^[[:space:]]*build[[:space:]]+-trimpath=true$' "$AUDIT_DIR/buildinfo" || {
            echo "Android mobile core was built without -trimpath" >&2
            exit 1
        }
        grep -Fq ": go$GO_VERSION" "$AUDIT_DIR/buildinfo" || {
            echo "Android mobile core was not built with required Go $GO_VERSION" >&2
            exit 1
        }
        ;;
    *.xcframework)
        FRAMEWORK_BINARY="$ARTIFACT/ios-arm64/Mobilecore.framework/Mobilecore"
        lipo "$FRAMEWORK_BINARY" -thin arm64 -output "$AUDIT_DIR/mobilecore.a"
        (cd "$AUDIT_DIR" && ar -x mobilecore.a)
        strings "$AUDIT_DIR/go.o" >"$AUDIT_DIR/buildinfo"
        awk 'previous ~ /^[A-Za-z0-9.-]+\.[A-Za-z0-9.-]+\// && $0 ~ /^v[0-9]/ { print previous "@" $0 } { previous = $0 }' \
            "$AUDIT_DIR/buildinfo" |
            sed '/^github.com\/bojieli\/queqiao@/d; /^github.com\/bojieli\/queqiao\/mobile\/core@/d' |
            sort -u >"$AUDIT_DIR/actual"
        grep -Fqx -- '-trimpath=true' "$AUDIT_DIR/buildinfo" || {
            echo "iOS mobile core was built without -trimpath" >&2
            exit 1
        }
        grep -Fqx -- "go$GO_VERSION" "$AUDIT_DIR/buildinfo" || {
            echo "iOS mobile core was not built with required Go $GO_VERSION" >&2
            exit 1
        }
        ;;
    *)
        echo "unsupported mobile artifact: $ARTIFACT" >&2
        exit 2
        ;;
esac

if grep -Fq "$PROJECT_DIR" "$AUDIT_DIR/buildinfo"; then
    echo "mobile artifact leaks the local checkout path" >&2
    exit 1
fi

sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' "$LOCK_FILE" |
    cut -d '|' -f 1 | sort -u >"$AUDIT_DIR/expected"
if ! diff -u "$AUDIT_DIR/expected" "$AUDIT_DIR/actual"; then
    echo "compiled mobile dependency graph differs from the reviewed runtime lock" >&2
    exit 1
fi

echo "Compiled mobile dependency graph and trim-path setting verified."
