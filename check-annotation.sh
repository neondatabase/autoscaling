#!/bin/bash

set -eu -o pipefail

ANNOTATION="$1"

while sleep 2; do
    date +"%Y-%m-%d %H:%M:%S"
    echo Without
    kubectl get pod -l vm.neon.tech/name -o json | jq ".items[] | select(.metadata.annotations[\"$ANNOTATION\"] == null) | .metadata.name" | wc -l
    echo With
    kubectl get pod -l vm.neon.tech/name -o json | jq ".items[] | select(.metadata.annotations[\"$ANNOTATION\"] != null) | .metadata.name" | wc -l
    echo
done
