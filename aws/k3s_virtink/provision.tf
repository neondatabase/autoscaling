resource "random_string" "token" {
  length  = 20
  special = false
}

locals {
  vm_cidr       = "10.77.0.0/16"
  vm_dhcp_start = "10.77.1.0"
  vm_dhcp_end   = "10.77.8.0"
  networking    = file("${path.module}/scripts/networking.sh")
  k3s_bootstrap = file("${path.module}/scripts/k3s_bootstrap.sh")
  k3s_join      = file("${path.module}/scripts/k3s_join.sh")
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

resource "null_resource" "k3s_boostrap" {
  depends_on = [
    aws_instance.this,
    null_resource.overlay_net
  ]

  # Changes in instances requires re-provisioning
  triggers = {
    instance_id = aws_instance.this[0].id
    script      = local.k3s_bootstrap
  }

  connection {
    type  = "ssh"
    agent = false

    host        = aws_instance.this[0].public_ip
    user        = "ubuntu"
    private_key = var.ssh_private_key
  }

  provisioner "file" {
    content     = local.k3s_bootstrap
    destination = "/tmp/k3s_bootstrap.sh"
  }

  provisioner "remote-exec" {
    inline = [
      "bash /tmp/k3s_bootstrap.sh ${random_string.token.result} ${split("/", local.vm_cidr)[0]} ${cidrnetmask(local.vm_cidr)} ${local.vm_dhcp_start} ${local.vm_dhcp_end} ${cidrhost(local.vm_cidr, 1)}",
    ]
  }
}

resource "null_resource" "k3s_join" {
  depends_on = [
    aws_instance.this,
    null_resource.overlay_net,
    null_resource.k3s_boostrap
  ]

  count = var.cluster_size - 1

  # Changes in instances requires re-provisioning
  triggers = {
    cluster_address = aws_instance.this[0].private_ip
    instance_id     = aws_instance.this[count.index + 1].id
    script          = local.k3s_join
  }

  connection {
    type  = "ssh"
    agent = false

    host        = aws_instance.this[count.index + 1].public_ip
    user        = "ubuntu"
    private_key = var.ssh_private_key
  }

  provisioner "file" {
    content     = local.k3s_join
    destination = "/tmp/k3s_join.sh"
  }

  provisioner "remote-exec" {
    inline = [
      "bash /tmp/k3s_join.sh ${aws_instance.this[0].private_ip} ${random_string.token.result}",
    ]
  }
}
