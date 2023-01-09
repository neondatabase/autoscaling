#!/usr/bin/env bash
#
# Runs a local docker registry for faster development.
#
# This script is based on <https://kind.sigs.k8s.io/docs/user/local-registry/>

set -eu -o pipefail

reg_name='kind-registry'
reg_port='5001'

# Start the registry if it's not already running
if [ "$(docker inspect -f '{{.State.Running}}' "$reg_name" 2>/dev/null || true)" != 'true' ]; then
    docker run \
        -d --restart=always -p "127.0.0.1:$reg_port:5000" --name "$reg_name" \
        registry:2
fi

# Connect the registry to the cluster if it isn't already
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${reg_name}")" = 'null' ]; then
    docker network connect "kind" "$reg_name"
fi
