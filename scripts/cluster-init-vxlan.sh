#!/usr/bin/env bash

set -eux -o pipefail

# Allow this script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"
cd .. # but all of the references are to things in the upper directory

source './scripts-common.sh'

BRIDGE_NAME='vm-bridge'
VXLAN_NAME='vm-vxlan0'

nodenames=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
nodeips="$( \
    kubectl get nodes -o jsonpath='{.items[*].status.addresses}' \
        | jq -sr 'map(map(select(.type == "InternalIP")) | .[0].address) | join(" ")' \
)"

get_node_ip () {
    kubectl get node "$1" -o jsonpath='{.status.addresses}' \
        | jq -r 'map(select(.type == "InternalIP") | .address)[0]'
}

for nodename in $nodenames; do
    echo "setting up network for $nodename..."

    node_ip="$(get_node_ip "$nodename")"
    echo "> node_ip = $node_ip"

    docker cp scripts/start-vm-bridge.sh "$nodename:/etc/start-vm-bridge.sh"
    docker container exec -it "$nodename" \
        /bin/bash /etc/start-vm-bridge.sh "$BRIDGE_NAME" "$VXLAN_NAME" "$node_ip" "$nodeips" \
        | indent
done

