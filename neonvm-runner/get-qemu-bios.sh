#!/usr/bin/env bash
#
USAGE="$0 <arch> <OS>"
#
# Helper script for fetching the appropriate firmware. If firmware is required for the arch/OS, the
# file will be placed into "./external-firmware/$arch-$os/QEMU_EFI.fd".
#
# If the required file is already present, with the correct SHA256 hash, nothing will change.

ARM_LINARO_SHA='e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'

ARM64_DARWIN_BIOS_SHA='47765fe344818cbc464b1c14ae658fb4b854f5c2ceffa982411731eb4865594d'
ARM64_DARWIN_QEMU_VERSION='10.0.2_2'
# sha via:  curl https://formulae.brew.sh/api/formula/qemu.json | jq '.bottle.stable.files["arm64_sequoia"]'
ARM64_DARWIN_QEMU_SHA='adb87194ee5c7d14f3ab91c1013113cae741b6b35b74ca7d228b5e583e987d8a'

set -eu -o pipefail

main () {
    if [ "$#" -ne 2 ]; then
        echo "$USAGE" >/dev/stderr
        exit 1
    fi

    arch="$1"
    check_arg_value 'architecture' "$arch" 'amd64' 'arm64' # NOTE: these are provided by the makefile, we only accept a limited set.
    os="$2"
    check_arg_value 'OS' "$os" 'linux' 'darwin' # NOTE: these are provided by the makefile, we only accept a limited set.

    cd -P -- "$(dirname "$(realpath "$0")")"

    target_dir="external-firmware/$arch-$os"
    target_file="$target_dir/QEMU_EFI.fd"
    mkdir -p "$target_dir"

    if [ "$arch" = 'amd64' ]; then
        echo "No extra BIOS needed on x86_64"
        exit
    fi

    test_bins curl mktemp

    if [ "$os" = 'linux' ]; then
        download_linaro_aarch64 "$target_file"
    elif [ "$os" = 'darwin' ]; then
        download_qemu_applesilicon_aarch64 "$target_file"
    else
        echo "Internal error: unexpected OS" >/dev/stderr
        exit 1
    fi
}

# Usage: check_arg_value <name> <value> <potential values...>
check_arg_value () {
    name="$1"
    value="$2"
    shift; shift

    for v in "$@"; do
        if [ "$value" = "$v" ]; then
            return
        fi
    done

    printf -v values "'%s', " "$@"
    values="${values%, }"
    echo "Unexpected $name '$value', expected one of $values" >/dev/stderr
    echo "$USAGE" >/dev/stderr
    exit 1
}

test_bins () {
    for bin in "$@"; do
        if ! which "$bin" >/dev/null; then
            echo "Unable to find '$bin' in path" >/dev/stderr
            exit 1
        fi
    done
}

# Usage: download_linaro_aarch64 <target-file>
download_linaro_aarch64 () {
    file="$1"

    if [ -f "$file" ]; then
        sha256="$(sha256sum "$file" | cut -d' ' -f1)"
        if [ "$sha256" = "$ARM_LINARO_SHA" ]; then
            echo "File $file already exists with correct SHA. Nothing to do."
            return
        else
            echo "File $file already exists, but has incorrect SHA ($sha256 vs $ARM_LINARO_SHA)" >/dev/stderr
            exit 1
        fi
    fi

    curl -f "https://releases.linaro.org/components/kernel/uefi-linaro/16.02/release/qemu64/QEMU_EFI.fd" -o "$file"

    sha256="$(sha256sum "$file" | cut -d' ' -f1)"
    if [ "$sha256" = "$ARM_LINARO_SHA" ]; then
        echo "File downloaded and SHA matches expected ($ARM_LINARO_SHA)"
    else
        echo "File downloaded but SHA is incorrect ($sha256 vs $ARM_LINARO_SHA)" >/dev/stderr
        exit 1
    fi
}

# Usage: download_qemu_applesilicon_aarch64 <target-file>
download_qemu_applesilicon_aarch64 () {
    file="$1"

    if [ -f "$file" ]; then
        sha256="$(sha256sum "$file" | cut -d' ' -f1)"
        if [ "$sha256" = "$ARM64_DARWIN_BIOS_SHA" ]; then
            echo "File $file already exists with correct SHA. Nothing to do."
            return
        else
            echo "File $file already exists, but has incorrect SHA ($sha256 vs $ARM64_DARWIN_BIOS_SHA)" >/dev/stderr
            exit 1
        fi
    fi

    tmpfile="$(mktemp "/tmp/qemu_arm64_sequoia-XXXXXXX.tar.gz")"
    trap "rm -f $tmpfile" EXIT

    url="https://ghcr.io/v2/homebrew/core/qemu/blobs/sha256:$ARM64_DARWIN_QEMU_SHA"

    echo "Fetching '$url' -> "$tmpfile""
    curl -fL \
        -H 'Authorization: Bearer QQ==' \
        "$url" \
        -o "$tmpfile"

    sha256="$(sha256sum "$tmpfile" | cut -d' ' -f1)"
    if [ "$sha256" != "$ARM64_DARWIN_QEMU_SHA" ]; then
        echo "Blob for homebrew qemu download has incorrect SHA ($sha256 vs $ARM64_DARWIN_QEMU_SHA)" >/dev/stderr
        exit 1
    fi

    echo "Extracting 'qemu/$ARM64_DARWIN_QEMU_VERSION/share/qemu/edk2-aarch64-code.fd' from tar"
    tar -zxv --to-stdout -f "$tmpfile" "qemu/$ARM64_DARWIN_QEMU_VERSION/share/qemu/edk2-aarch64-code.fd" > "$file"

    sha256="$(sha256sum "$file" | cut -d' ' -f1)"
    if [ "$sha256" = "$ARM64_DARWIN_BIOS_SHA" ]; then
        echo "File downloaded and SHA matches expected ($ARM64_DARWIN_BIOS_SHA)"
    else
        echo "File downloaded but SHA is incorrect ($sha256 vs $ARM64_DARWIN_BIOS_SHA)" >/dev/stderr
        exit 1
    fi
}

main "$@"
