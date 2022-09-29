#!/bin/bash

echo "wait for cloud-init finish"
cloud-init status --wait >/dev/null

BRIDGE_IP=$1
VM_CIDR=$2
IP_LIST=$3

BRIDGE_NAME="vm-bridge0"
MYIP=$(hostname -i)

VXLAN_NAME="vm-vxlan0"

VXLAN_PEER_LIST=$(echo ${IP_LIST} | sed "s/${MYIP}//g" | sed 's/,/ /g')

echo "create bridge ${BRIDGE_NAME} interface if not exist"
ip link show dev ${BRIDGE_NAME} >/dev/null 2>&1 || sudo ip link add name ${BRIDGE_NAME} type bridge
sudo ip address add ${BRIDGE_IP} dev ${BRIDGE_NAME}
sudo ip link set dev ${BRIDGE_NAME} up

echo "create ${VXLAN_NAME} interface if not exist"
ip link show dev ${VXLAN_NAME} >/dev/null 2>&1 || sudo ip link add ${VXLAN_NAME} type vxlan id 100 local ${MYIP} dstport 4789
sudo ip link set ${VXLAN_NAME} master ${BRIDGE_NAME}
sudo ip link set ${VXLAN_NAME} up
for i in ${VXLAN_PEER_LIST}; do
  echo "appending ${i} to vxlan FDB"
  sudo bridge fdb append 00:00:00:00:00:00 dev ${VXLAN_NAME} dst ${i}
done

echo "setup NAT"
sudo iptables -t nat -A POSTROUTING -s ${VM_CIDR}  ! -d ${VM_CIDR} -j MASQUERADE
sudo iptables        -A FORWARD -i ${BRIDGE_NAME} -j ACCEPT
sudo iptables        -A FORWARD -o ${BRIDGE_NAME} -m state --state RELATED,ESTABLISHED -j ACCEPT
