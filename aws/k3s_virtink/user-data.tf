# it will install necessary packages

locals {
  user_data = <<-EOT
  #cloud-config
  package_update: true
  packages:
    - curl
    - ca-certificates
    - jq
    - awscli
    - nfs-common
    - open-iscsi
    - bash-completion
  runcmd:
    - sysctl -w net.ipv4.ip_forward=1
  EOT
}
