#!/bin/bash

### setup K3S worker node
###
### input parameters:
###
### $1 - hostname (string)
### $2 - k3s token (string)
### $3 - k3s master node ip (string)
### $4 - private IP's of all nodes in cluster (comma separated string)

NODENAME=$1
TOKEN=$2
MASTER=$3
ALLIP=$4

if [ -f /run/provision_status ]; then
    # seems already provisoned ? checking
    if [ "$(cat /run/provision_status)" = "provisioned" ]; then
        # no need repeat provision, exiting
        echo "instance already provisioned, exiting"
        exit
    fi
fi

_echo() {
    echo "$(tput setaf 2)$@$(tput sgr0)"
}

# system wide stuff

_echo "setup hostname"
sudo hostnamectl set-hostname --static ${NODENAME}
echo

_echo "install necessary deps"
while ! sudo apt-get -qq update; do
    echo "updating the package information"
done
sudo DEBIAN_FRONTEND=noninteractive apt-get install -qq --no-install-recommends curl ca-certificates jq awscli nfs-common open-iscsi bash-completion </dev/null >/dev/null
echo

_echo "allow routing"
sudo sysctl -w net.ipv4.ip_forward=1
echo

# VXLAN overlay networking stuff

BRIDGE_NAME="vm-bridge0"
VXLAN_NAME="vm-vxlan0"
MYIP=$(curl -s http://169.254.169.254/latest/meta-data/local-ipv4)

VXLAN_PEER_LIST=$(echo ${ALLIP} | sed "s/${MYIP}//g" | sed 's/,/ /g')

_echo "create linux bridge interface '${BRIDGE_NAME}' if not exist"
ip link show dev ${BRIDGE_NAME} >/dev/null 2>&1 || sudo ip link add name ${BRIDGE_NAME} type bridge
sudo ip link set dev ${BRIDGE_NAME} up
echo

_echo "create VXLAN interface '${VXLAN_NAME}' if not exist"
ip link show dev ${VXLAN_NAME} >/dev/null 2>&1 || sudo ip link add ${VXLAN_NAME} type vxlan id 100 local ${MYIP} dstport 4789
sudo ip link set ${VXLAN_NAME} master ${BRIDGE_NAME}
sudo ip link set ${VXLAN_NAME} up
for i in ${VXLAN_PEER_LIST}; do
  echo "appending ${i} to VXLAN FDB"
  sudo bridge fdb append 00:00:00:00:00:00 dev ${VXLAN_NAME} dst ${i}
done
echo

# kubernetes stuff

_echo "install cni plugins"
sudo mkdir -p /opt/cni/bin
curl -sfL https://github.com/containernetworking/plugins/releases/download/v1.1.1/cni-plugins-linux-amd64-v1.1.1.tgz -o - | sudo tar xzf - -C /opt/cni/bin
echo

_echo "join to cluster"
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable K3S_URL=https://${MASTER}:6443 K3S_TOKEN=${TOKEN} sudo -E -H sh -s - \
    --kubelet-arg 'system-reserved=cpu=500m,memory=1024Mi,ephemeral-storage=1Gi' \
    --kubelet-arg 'kube-reserved=cpu=200m,memory=256Mi'
echo

_echo "provisioned" | sudo tee /run/provision_status
echo
