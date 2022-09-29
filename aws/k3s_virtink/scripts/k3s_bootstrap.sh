#!/bin/bash

echo "wait for cloud-init finish"
cloud-init status --wait

TOKEN=$1
VM_NET=$2
VM_MASK=$3
VM_DHCP_START=$4
VM_DHCP_END=$5
GATEWAY=$6
#EFS_ID=$7

BRIDGE_NAME="vm-bridge0"

echo "setup DHCP server"
sudo apt install -y isc-dhcp-server
cat <<EOF | sudo tee /etc/default/isc-dhcp-server
INTERFACESv4="${BRIDGE_NAME}"
INTERFACESv6=""
EOF
cat <<EOF | sudo tee /etc/dhcp/dhcpd.conf
# lease time -1 mean infinity
default-lease-time 86400;
max-lease-time 86400;

subnet ${VM_NET} netmask ${VM_MASK} {
  range ${VM_DHCP_START} ${VM_DHCP_END};
  option routers ${GATEWAY};
  option domain-name-servers 8.8.8.8, 1.1.1.1;
}
EOF
sleep 5
sudo systemctl restart isc-dhcp-server

echo "install cni plugins"
sudo mkdir -p /opt/cni/bin
curl -sfL https://github.com/containernetworking/plugins/releases/download/v1.1.1/cni-plugins-linux-amd64-v1.1.1.tgz -o - | sudo tar xzf - -C /opt/cni/bin

echo "bootstap cluster"
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable K3S_TOKEN=${TOKEN} sudo -E -H sh -s - \
    --tls-san $(curl -s http://169.254.169.254/latest/meta-data/public-ipv4) \
    --flannel-backend=none \
    --disable-network-policy \
    --cluster-cidr=192.168.0.0/16 \
    --disable traefik \
    --node-label svccontroller.k3s.cattle.io/enablelb=false \
    --kubelet-arg 'system-reserved=cpu=500m,memory=1024Mi,ephemeral-storage=1Gi' \
    --kubelet-arg 'kube-reserved=cpu=200m,memory=256Mi'

echo "adding addons"
while ! sudo test -d /var/lib/rancher/k3s/server/manifests; do
  echo "waiting for /var/lib/rancher/k3s/server/manifests"
  sleep 1
done
sudo curl -sfL https://raw.githubusercontent.com/projectcalico/calico/v3.24.1/manifests/calico.yaml -o /var/lib/rancher/k3s/server/manifests/calico.yaml
sudo curl -sfL https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset.yml -o /var/lib/rancher/k3s/server/manifests/multus-daemonset.yml
sudo curl -sfL https://github.com/cert-manager/cert-manager/releases/download/v1.9.1/cert-manager.yaml -o /var/lib/rancher/k3s/server/manifests/cert-manager.yaml
sudo curl -sfL https://raw.githubusercontent.com/longhorn/longhorn/v1.3.1/deploy/longhorn.yaml -o /var/lib/rancher/k3s/server/manifests/longhorn.yaml

#sudo kubectl kustomize "github.com/kubernetes-sigs/aws-efs-csi-driver/deploy/kubernetes/overlays/stable/?ref=release-1.3" -o /var/lib/rancher/k3s/server/manifests/efs-csi.yaml
#sudo curl -sfL https://github.com/kubevirt/containerized-data-importer/releases/download/v1.55.0/cdi-operator.yaml -o /var/lib/rancher/k3s/server/manifests/cdi-operator.yaml
#sudo curl -sfL https://github.com/kubevirt/containerized-data-importer/releases/download/v1.55.0/cdi-cr.yaml -o /var/lib/rancher/k3s/server/manifests/cdi-cr.yaml

#cat <<EOF | sudo tee /var/lib/rancher/k3s/server/manifests/pv.yaml
#kind: StorageClass
#apiVersion: storage.k8s.io/v1
#metadata:
#  name: efs
#provisioner: efs.csi.aws.com
#parameters:
#  provisioningMode: efs-ap
#  fileSystemId: $EFS_ID
#  #directoryPerms: "700"
#  #gidRangeStart: "1000" # optional
#  #gidRangeEnd: "2000" # optional
#  #basePath: "/dynamic_provisioning" # optional
#EOF
echo "done"
