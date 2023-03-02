#!/usr/bin/env bash
#
# Personal helper script to download deployment files for a version.

if [[ "$#" != "1" ]]; then
    echo "USAGE: ./download.sh <VERSION>"
    exit 1
fi

VERSION="$1"

curl -fL "https://github.com/neondatabase/autoscaling/releases/download/$VERSION/autoscale-scheduler.yaml" -o "autoscale-scheduler-$VERSION.yaml"
curl -fL "https://github.com/neondatabase/autoscaling/releases/download/$VERSION/autoscaler-agent.yaml" -o "autoscaler-agent-$VERSION.yaml"
