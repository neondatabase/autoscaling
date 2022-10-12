resource "random_string" "password" {
  length  = 20
  special = false
}

locals {
  vm_cidr       = "10.42.0.0/16"
  vm_dhcp_start = "10.42.1.0"
  vm_dhcp_end   = "10.42.8.0"
  networking    = file("${path.module}/scripts/networking.sh")
  lxd_bootstrap = file("${path.module}/scripts/lxd_bootstrap.sh")
  lxd_join      = file("${path.module}/scripts/lxd_join.sh")
}


resource "null_resource" "overlay_net" {
  depends_on = [
    aws_instance.this
  ]

  count = var.cluster_size

  # Changes in instances requires re-provisioning
  triggers = {
    instance_id = aws_instance.this[count.index].id
    script      = local.networking
  }

  connection {
    type  = "ssh"
    agent = false

    host        = aws_instance.this[count.index].public_ip
    user        = "ubuntu"
    private_key = var.ssh_private_key
  }

  provisioner "file" {
    content     = local.networking
    destination = "/tmp/networking.sh"
  }

  provisioner "remote-exec" {
    inline = [
      "bash /tmp/networking.sh ${cidrhost(local.vm_cidr, count.index + 1)}/${split("/", local.vm_cidr)[1]} ${local.vm_cidr} ${join(",", aws_instance.this[*].private_ip)}",
    ]
  }
}


resource "null_resource" "lxd_boostrap" {
  depends_on = [
    aws_instance.this,
    null_resource.overlay_net
  ]

  # Changes in instances requires re-provisioning
  triggers = {
    instance_id = aws_instance.this[0].id
    script      = local.lxd_bootstrap
  }

  connection {
    type  = "ssh"
    agent = false

    host        = aws_instance.this[0].public_ip
    user        = "ubuntu"
    private_key = var.ssh_private_key
  }

  provisioner "file" {
    content     = local.lxd_bootstrap
    destination = "/tmp/lxd_bootstrap.sh"
  }

  provisioner "remote-exec" {
    inline = [
      "bash /tmp/lxd_bootstrap.sh ${random_string.password.result} ${aws_s3_bucket.this.id} ${split("/", local.vm_cidr)[0]} ${cidrnetmask(local.vm_cidr)} ${local.vm_dhcp_start} ${local.vm_dhcp_end} ${cidrhost(local.vm_cidr, 1)}",
    ]
  }
}

resource "null_resource" "lxd_join" {
  depends_on = [
    aws_instance.this,
    null_resource.overlay_net,
    null_resource.lxd_boostrap
  ]

  count = var.cluster_size - 1

  # Changes in instances requires re-provisioning
  triggers = {
    cluster_address = aws_instance.this[0].private_ip
    instance_id     = aws_instance.this[count.index + 1].id
    script          = local.lxd_join
  }

  connection {
    type  = "ssh"
    agent = false

    host        = aws_instance.this[count.index + 1].public_ip
    user        = "ubuntu"
    private_key = var.ssh_private_key
  }

  provisioner "file" {
    content     = local.lxd_join
    destination = "/tmp/lxd_join.sh"
  }

  provisioner "remote-exec" {
    inline = [
      "bash /tmp/lxd_join.sh ${aws_instance.this[0].private_ip} ${random_string.password.result} ${aws_s3_bucket.this.id}",
    ]
  }
}
