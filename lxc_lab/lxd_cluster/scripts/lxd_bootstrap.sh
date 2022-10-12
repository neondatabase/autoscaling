#!/bin/bash

echo "wait for cloud-init finish"
cloud-init status --wait

PASSWORD=$1
BUCKET=$2
VM_NET=$3
VM_MASK=$4
VM_DHCP_START=$5
VM_DHCP_END=$6
GATEWAY=$7

NAME=$(hostname -s)
MYIP=$(curl -s http://169.254.169.254/latest/meta-data/local-ipv4)
BRIDGE_NAME="vm-bridge0"

echo "setup DHCP server"
sudo apt install -y isc-dhcp-server
cat <<EOF | sudo tee /etc/default/isc-dhcp-server
INTERFACESv4="${BRIDGE_NAME}"
INTERFACESv6=""
EOF
cat <<EOF | sudo tee /etc/dhcp/dhcpd.conf
default-lease-time 600;
max-lease-time 7200;

subnet ${VM_NET} netmask ${VM_MASK} {
  range ${VM_DHCP_START} ${VM_DHCP_END};
  option routers ${GATEWAY};
  option domain-name-servers 8.8.8.8, 1.1.1.1;
}
EOF
sudo systemctl restart isc-dhcp-server


echo "bootstap cluster"
cat <<EOF | sudo lxd init --preseed
cluster:
  server_name: ${NAME}
  enabled: true

config:
  core.trust_password: ${PASSWORD}
  core.https_address: ${MYIP}:8443

storage_pools:
- config:
    size: 64GB
  description: ""
  name: local
  driver: zfs

networks: []

profiles:
- config:
    cloud-init.user-data: |
      #cloud-config
      package_upgrade: true
      packages:
        - wget
        - nano
      runcmd:
        - sh -c 'echo "deb http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list'
        - wget --quiet -O - https://www.postgresql.org/media/keys/ACCC4CF8.asc | sudo apt-key add -
        - apt-get update
        - apt-get -y install postgresql-14
        - echo "listen_addresses = '*'" | tee -a /etc/postgresql/14/main/postgresql.conf
        - echo "max_wal_senders = 0" | tee -a /etc/postgresql/14/main/postgresql.conf
        - echo "wal_level = minimal" | tee -a /etc/postgresql/14/main/postgresql.conf
        - echo "#fsync = off" | tee -a /etc/postgresql/14/main/postgresql.conf
        - echo "host all all 0.0.0.0/0 trust" | tee -a /etc/postgresql/14/main/pg_hba.conf
        - su - postgres -c 'PGPASSWORD=bench createuser -s bench'
        - su - postgres -c 'createdb bench'
        - systemctl restart postgresql@14-main
    limits.cpu: "4"
    limits.memory: 4GB
    limits.disk.priority: 10
    limits.network.priority: 10
    migration.stateful: "true"
    security.secureboot: "false"
    cluster.evacuate: live-migrate
  description: ""
  devices:
    eth0:
      name: eth0
      nictype: bridged
      parent: ${BRIDGE_NAME}
      type: nic
    root:
      path: /
      pool: local
      type: disk
      size: 8GB
      size.state: 4GB
  name: default
EOF
sleep 10

echo "put cluster cert to s3"
aws s3 cp /var/snap/lxd/common/lxd/cluster.crt s3://${BUCKET}/lxd/cluster.crt

echo "done"
