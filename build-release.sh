#!/usr/bin/env bash
# Build the release artifacts the CLIProxyAPI plugin store expects.
#
# The store requires one archive per supported platform:
#
#   cliproxy-tracing_<version>_<goos>_<goarch>.zip
#
# for darwin/amd64, darwin/arm64, linux/amd64, linux/arm64, windows/amd64 and
# windows/arm64, plus one sha256sum-format checksums.txt. Each archive holds the
# library at its root under the name the store documents: cliproxy-tracing.<ext>.
#
# Every required platform is mandatory. A missing toolchain is an error, never a
# silent skip: a release that quietly drops a platform is exactly the failure
# this script exists to prevent. Build darwin targets on macOS (there is no
# portable cross toolchain for them), windows/amd64 on Windows or with mingw,
# linux/arm64 with aarch64-linux-gnu-gcc, and windows/arm64 with zig, because
# mingw only targets x86.
#
# Usage:
#   ./build-release.sh                    build every platform, then package
#   TARGET=linux/amd64 ./build-release.sh build one platform into dist/lib/
#   ./build-release.sh --package          package the libraries already built
#
# Environment:
#   VERSION   release version without the leading v (default: 0.2.2)
#   OUT_DIR   output directory (default: dist)
#   CC_<goos>_<goarch>   override the compiler for one platform, for example
#                        CC_linux_arm64=aarch64-linux-gnu-gcc
set -euo pipefail

cd "$(dirname "$0")"

PLUGIN_ID="cliproxy-tracing"
VERSION="${VERSION:-0.2.2}"
VERSION="${VERSION#v}"
OUT_DIR="${OUT_DIR:-dist}"
LIB_DIR="${OUT_DIR}/lib"

PLATFORMS=(
    darwin/amd64
    darwin/arm64
    linux/amd64
    linux/arm64
    windows/amd64
    windows/arm64
)

lib_ext() {
    case "$1" in
        darwin) echo ".dylib" ;;
        windows) echo ".dll" ;;
        *) echo ".so" ;;
    esac
}

have_cc() {
    command -v "$1" >/dev/null 2>&1
}

# cc_candidates lists the compilers to try for a platform, best first. An
# explicit CC_<goos>_<goarch> wins; then the host compiler on the host platform;
# then known cross compilers by name. Every candidate is probed with command -v,
# so a name that does not exist on a particular runner is skipped rather than
# assumed.
cc_candidates() {
    local goos="$1" goarch="$2" host_os host_arch
    host_os="$(go env GOHOSTOS)"
    host_arch="$(go env GOHOSTARCH)"

    local override="CC_${goos}_${goarch}"
    if [ -n "${!override:-}" ]; then
        echo "${!override}"
        return
    fi

    if [ "${goos}" = "${host_os}" ] && [ "${goarch}" = "${host_arch}" ]; then
        case "${goos}" in
            windows) echo "x86_64-w64-mingw32-gcc gcc cc" ;;
            *) echo "${CC:-cc} gcc" ;;
        esac
        return
    fi

    case "${goos}/${goarch}" in
        linux/arm64) echo "aarch64-linux-gnu-gcc" ;;
        windows/amd64) echo "x86_64-w64-mingw32-gcc" ;;
        # No gcc targets aarch64-windows-gnu; zig ships its own libc for the
        # target and speaks the mingw ABI. zigcc is a small wrapper the release
        # workflow installs around `zig cc -target aarch64-windows-gnu`.
        windows/arm64) echo "aarch64-w64-mingw32-gcc zigcc" ;;
        darwin/*) echo "clang" ;;
        *) echo "" ;;
    esac
}

build_one() {
    local platform="$1" goos goarch cc="" candidate stage lib
    goos="${platform%%/*}"
    goarch="${platform##*/}"

    for candidate in $(cc_candidates "${goos}" "${goarch}"); do
        if have_cc "${candidate}"; then
            cc="${candidate}"
            break
        fi
    done

    if [ -z "${cc}" ]; then
        echo "error: ${platform}: no compiler found (tried: $(cc_candidates "${goos}" "${goarch}"))" >&2
        echo "       install one or set CC_${goos}_${goarch}" >&2
        return 1
    fi

    stage="${LIB_DIR}/${goos}_${goarch}"
    mkdir -p "${stage}"
    lib="${PLUGIN_ID}-v${VERSION}$(lib_ext "${goos}")"

    echo "building ${platform} with ${cc}"
    CC="${cc}" CGO_ENABLED=1 GOOS="${goos}" GOARCH="${goarch}" \
        go build -trimpath -buildmode=c-shared \
        -ldflags "-s -w -X main.pluginVersion=${VERSION}" \
        -o "${stage}/${lib}" .

    # A build can succeed and still produce the wrong format when the compiler
    # ignores GOOS, so check the binary itself.
    go version -m "${stage}/${lib}" >/dev/null 2>&1 || {
        echo "error: ${platform}: ${lib} is not a readable Go binary" >&2
        return 1
    }
    # c-shared also emits a C header beside the library. It is not part of the
    # release and must not reach the archive.
    rm -f "${stage%/}/${lib%.*}.h"
    echo "built ${stage}/${lib} ($(wc -c <"${stage}/${lib}") bytes)"
}

# package_all zips every built library and fails unless all required platforms
# are present. Publishing an incomplete matrix is never a valid outcome.
package_all() {
    local missing=() platform goos goarch lib zip_name

    for platform in "${PLATFORMS[@]}"; do
        goos="${platform%%/*}"
        goarch="${platform##*/}"
        lib="${LIB_DIR}/${goos}_${goarch}/${PLUGIN_ID}-v${VERSION}$(lib_ext "${goos}")"
        if [ ! -f "${lib}" ]; then
            missing+=("${platform}")
        fi
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        echo "error: missing libraries for: ${missing[*]}" >&2
        echo "       build each one with TARGET=<goos>/<goarch> ./build-release.sh" >&2
        return 1
    fi

    for platform in "${PLATFORMS[@]}"; do
        goos="${platform%%/*}"
        goarch="${platform##*/}"
        built="${PLUGIN_ID}-v${VERSION}$(lib_ext "${goos}")"
        # The store documents the archive entry as <id>.<ext>, and every
        # published plugin uses that name. The installer also accepts the
        # versioned form, but the plain name is the documented contract, so
        # stage a copy under it rather than shipping the versioned filename.
        entry="${PLUGIN_ID}$(lib_ext "${goos}")"
        zip_name="${PLUGIN_ID}_${VERSION}_${goos}_${goarch}.zip"
        stage="${LIB_DIR}/${goos}_${goarch}/.package"
        rm -rf "${stage}" && mkdir -p "${stage}"
        cp "${LIB_DIR}/${goos}_${goarch}/${built}" "${stage}/${entry}"
        rm -f "${OUT_DIR}/${zip_name}"
        # Build the archive from inside the staging directory so the library
        # lands at the zip root, which is what the host installer requires.
        (cd "${stage}" && zip -q -X "../../../${zip_name}" "${entry}")
        rm -rf "${stage}"
        echo "packaged ${OUT_DIR}/${zip_name} (entry: ${entry})"
    done

    # Checksums cover exactly the archives just packaged. A glob over *.zip
    # would also pick up leftovers from an earlier version or an earlier plugin
    # id, and those would then be published alongside the release.
    local archives=()
    for platform in "${PLATFORMS[@]}"; do
        goos="${platform%%/*}"
        goarch="${platform##*/}"
        archives+=("${PLUGIN_ID}_${VERSION}_${goos}_${goarch}.zip")
    done
    # The host looks checksums up by exact asset name, so no "./" prefix.
    (cd "${OUT_DIR}" && sha256sum "${archives[@]}" >checksums.txt)
    echo "---"
    (cd "${OUT_DIR}" && ls -l "${archives[@]}" checksums.txt)
}

usage() {
    # Print the comment header, stopping at the first non-comment line.
    awk '/^#!/ { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
}

main() {
    local arg="${1:-}"

    if [ "${arg}" = "-h" ] || [ "${arg}" = "--help" ]; then
        usage
        return 0
    fi

    mkdir -p "${OUT_DIR}"

    if [ "${arg}" = "--package" ]; then
        package_all
        return
    fi

    if [ -n "${TARGET:-}" ]; then
        build_one "${TARGET}"
        return
    fi

    # No target: build everything, failing on any missing toolchain rather than
    # producing a partial release.
    local failed=() platform
    for platform in "${PLATFORMS[@]}"; do
        if ! build_one "${platform}"; then
            failed+=("${platform}")
        fi
    done
    if [ "${#failed[@]}" -gt 0 ]; then
        echo "error: could not build: ${failed[*]}" >&2
        return 1
    fi
    package_all
}

main "$@"
