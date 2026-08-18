#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
MOBILE_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
CORE_DIR="$MOBILE_DIR/core"
RUNTIME_LOCK="$MOBILE_DIR/runtime-dependencies.lock"
BUILD_LOCK="$MOBILE_DIR/build-tools.lock"
ANDROID_BUILD_FILE="$MOBILE_DIR/android/build.gradle"
GRADLE_WRAPPER="$MOBILE_DIR/android/gradle/wrapper/gradle-wrapper.properties"
GRADLE_VERIFICATION="$MOBILE_DIR/android/gradle/verification-metadata.xml"

AUDIT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/queqiao-dependency-audit.XXXXXX")
trap 'rm -rf "$AUDIT_DIR"' EXIT HUP INT TERM

cd "$CORE_DIR"
go mod verify

# Go's module graph includes upstream projects' test and repository tooling.
# Only modules reached by compiled packages can enter the application binary,
# so derive the shipped graph from go list -deps and compare it exactly.
go list -deps -f '{{with .Module}}{{if and .Version (ne .Path "github.com/bojieli/queqiao")}}{{.Path}}@{{.Version}}{{end}}{{end}}' . |
    sed '/^$/d' | sort -u >"$AUDIT_DIR/runtime.actual"
sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' "$RUNTIME_LOCK" |
    cut -d '|' -f 1 | sed '/^golang.org\/x\/mobile@/d' | sort -u >"$AUDIT_DIR/runtime.expected"
if ! diff -u "$AUDIT_DIR/runtime.expected" "$AUDIT_DIR/runtime.actual"; then
    echo "mobile runtime dependency graph differs from the reviewed lock" >&2
    exit 1
fi

while IFS='|' read -r module_ref license_id maintenance_source; do
    case "$module_ref" in
        ''|'#'*) continue ;;
    esac
    case "$license_id" in
        MIT|BSD-3-Clause|Apache-2.0) ;;
        *)
            echo "unapproved license for $module_ref: $license_id" >&2
            exit 1
            ;;
    esac
    case "$maintenance_source" in
        https://*) ;;
        *)
            echo "missing maintenance source for $module_ref" >&2
            exit 1
            ;;
    esac
    module_name=${module_ref%@*}
    module_version=${module_ref##*@}
    module_dir=$(go list -m -f '{{.Dir}}' "$module_name@$module_version")
    if ! find "$module_dir" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' \) | grep -q .; then
        echo "module has no top-level license file: $module_ref" >&2
        exit 1
    fi
done <"$RUNTIME_LOCK"

go list -deps -f '{{with .Module}}{{if .Version}}{{.Path}}@{{.Version}}{{end}}{{end}}' \
    golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind |
    sed '/^$/d' | sort -u >"$AUDIT_DIR/tools.actual"
sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' "$BUILD_LOCK" |
    cut -d '|' -f 1 | sed -n '/^golang.org\/x\//p' | sort -u >"$AUDIT_DIR/tools.expected"
if ! diff -u "$AUDIT_DIR/tools.expected" "$AUDIT_DIR/tools.actual"; then
    echo "gomobile build-tool graph differs from the reviewed lock" >&2
    exit 1
fi

android_plugin_version=$(sed -n 's/.*com\.android\.application" version "\([^"]*\)".*/\1/p' "$ANDROID_BUILD_FILE")
gradle_version=$(sed -n 's|.*gradle-\([0-9][0-9.]*\)-bin\.zip|\1|p' "$GRADLE_WRAPPER")
if ! grep -Fqx "com.android.tools.build:gradle@$android_plugin_version|Apache-2.0|https://android.googlesource.com/platform/tools/base" "$BUILD_LOCK"; then
    echo "Android Gradle Plugin version is not in the reviewed build-tool lock" >&2
    exit 1
fi
if ! grep -Fqx "gradle@$gradle_version|Apache-2.0|https://github.com/gradle/gradle" "$BUILD_LOCK"; then
    echo "Gradle wrapper version is not in the reviewed build-tool lock" >&2
    exit 1
fi
if ! grep -Eq '^distributionSha256Sum=[0-9a-f]{64}$' "$GRADLE_WRAPPER"; then
    echo "Gradle distribution is missing its SHA-256 pin" >&2
    exit 1
fi
if ! grep -Fq '<verify-metadata>true</verify-metadata>' "$GRADLE_VERIFICATION" ||
   ! grep -Fq 'name="gradle" version="9.3.1"' "$GRADLE_VERIFICATION"; then
    echo "Gradle dependency verification metadata is missing or does not cover AGP 9.3.1" >&2
    exit 1
fi

echo "Mobile runtime and gomobile tool dependencies match the reviewed allowlists."
