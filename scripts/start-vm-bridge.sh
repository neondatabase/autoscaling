#!/usr/bin/env bash

set -ux -o pipefail

BRIDGE_NAME="$1"
VXLAN_NAME="$2"
node_ip="$3"
node_ips="$4"

echo "create linux bridge interface $BRIDGE_NAME if it doesn't exist"
ip link show dev "$BRIDGE_NAME" >/dev/null 2>&1 \
    || ip link add name "$BRIDGE_NAME" type bridge
ip link set dev "$BRIDGE_NAME" up

echo "create VXLAN interface $VXLAN_NAME if it doesn't exist"
ip link show dev "$VXLAN_NAME" >/dev/null 2>&1 \
    || ip link add "$VXLAN_NAME" type vxlan id 100 local "$node_ip" dstport 4789
ip link set "$VXLAN_NAME" master "$BRIDGE_NAME"
ip link set "$VXLAN_NAME" up

for peer_ip in $node_ips; do
    if [[ "$peer_ip" == "$node_ip" ]]; then
        continue
    fi

    echo "appending $peer_ip to VXLAN FDB"
    bridge fdb append 00:00:00:00:00:00 dev "$VXLAN_NAME" dst "$peer_ip"
done
