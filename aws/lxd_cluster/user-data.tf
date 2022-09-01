# it will install necessary packages

locals {
  user_data = <<-EOT
  #cloud-config
  package_update: true
  packages:
    - curl
    - jq
    - awscli
  runcmd:
    - snap refresh lxd --channel=latest/stable
    - sysctl -w net.ipv4.ip_forward=1
  EOT
}
