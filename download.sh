#!/usr/bin/env bash
#
# Personal helper script to download deployment files for a version.

set -eu -o pipefail

if [[ "$#" != "1" ]]; then
    echo "USAGE: ./download.sh <VERSION>"
    exit 1
fi

VERSION="$1"

download () {
    curl -fL "https://github.com/neondatabase/autoscaling/releases/download/$VERSION/$1.yaml" -o "$1-$VERSION.yaml"
}

download 'autoscale-scheduler'
download 'autoscaler-agent'
download 'neonvm'
download 'multus'
