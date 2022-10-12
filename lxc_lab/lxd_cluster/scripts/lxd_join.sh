#!/bin/bash

echo "wait for cloud-init finish"
cloud-init status --wait

CLUSTER_ADDRESS=$1
PASSWORD=$2
BUCKET=$3

echo "check cluster cert present in S3"
while ! aws s3api head-object --bucket ${BUCKET} --key lxd/cluster.crt >/dev/null 2>&1; do
  echo "waiting for cluster.crt"
  sleep 1
done

echo "try lxc"
lxc ls
sleep 5

echo "retrive cert"
sudo rm -f /var/snap/lxd/common/lxd/cluster.crt /var/snap/lxd/common/lxd/cluster.key
sudo aws s3 cp s3://${BUCKET}/lxd/cluster.crt /var/snap/lxd/common/lxd/cluster.crt
sudo openssl x509 -noout -text -in /var/snap/lxd/common/lxd/cluster.crt

echo "join to cluster"
cat <<EOF | sudo lxd init --preseed
cluster:
  enabled: true
  server_name: $(hostname -s)
  server_address: $(hostname -i):8443
  cluster_address: ${CLUSTER_ADDRESS}:8443
  cluster_password: "${PASSWORD}"
  cluster_certificate_path: /var/snap/lxd/common/lxd/cluster.crt
storage_pools:
- config:
    size: 64GB
  description: ""
  name: local
  driver: zfs
EOF

echo "done"
