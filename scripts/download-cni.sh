#!/usr/bin/env bash
#
# Helper script to download the CNI plugins into a local directory. This script must be run *before
# launching `kind`*.
#
# Alternatively, you can modify 'kind/config.yaml' to point to your system's CNI plugins, if they've
# been installed on the host.

# Allow the script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"
cd .. # but all of the references are to things in the upper directory

# Extract final binary files into 'cni-bin'
TARGET_DIR='kind/cni-bin'

check_dep () {
    if ! which "$1" >/dev/null 2>/dev/null; then
        echo "Missing required dependency $1"
        exit 1
    fi
}

check_dep uname
check_dep curl
check_dep jq
check_dep sha256sum
check_dep tar

set -eu -o pipefail

uname_arch="$(uname -m)"
case "$uname_arch" in
    x86_64)
        ARCH="amd64"
        ;;
    *)
        echo "Unknown architecture $uname_arch"
        echo "To add a new architecture to this script, check the architecture naming scheme of"
        echo "the assets in <https://github.com/containernetworking/plugins/releases/latest>"
        exit 1
        ;;
esac

REPO="containernetworking/plugins"

if [ -e "$TARGET_DIR" ]; then
    echo "directory $TARGET_DIR already exists"
    echo "aborting."
    exit 2
fi

echo "Fetching latest release from https://github.com/$REPO..."
release_info="$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest")"

# Find the assets corresponding to 'cni-plugins-linux-$ARCH-$VERSION.tgz[.sha256]'
JQ_FILTER="
.assets | .[]
    | select(.name | startswith(\"cni-plugins-linux-$ARCH\"))
    | { name: .name, url: .browser_download_url }"
# list of name, url objects for the .tgz and hashes, separated by newlines (i.e., not an array)
linux_group="$(echo "$release_info" | jq -e "$JQ_FILTER")"
tgz_file="$(echo "$linux_group" | jq -r 'select(.name | endswith(".tgz")) | .name')"
tgz_link="$(echo "$linux_group" | jq -r 'select(.name | endswith(".tgz")) | .url')"
sha256_link="$(echo "$linux_group" | jq -r 'select(.name | endswith(".sha256")) | .url')"

cleanup () {
    echo "cleaning up '$tgz_file'"
    rm "$tgz_file"
}

trap cleanup EXIT INT TERM

echo "Downloading '$tgz_link'..."
echo "  ... into '$tgz_file'"
curl -sSL "$tgz_link" -o "$tgz_file"

actual_sum="$(sha256sum "$tgz_file")"
echo "Downloading sha256 for '$tgz_file'..."
expected_sum="$(curl -sSL "$sha256_link")"

if [[ "$actual_sum" != "$expected_sum" ]]; then
    echo "Checksum was not valid. Expected:"
    echo " >> $expected_sum"
    echo "Found:"
    echo " >> $actual_sum"
    exit 1
fi

echo "Unpacking into $TARGET_DIR..."
mkdir "$TARGET_DIR"
tar -xf "$tgz_file" -C "$TARGET_DIR"
