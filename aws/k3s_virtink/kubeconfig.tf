resource "local_file" "setkubeconfig" {
  depends_on = [
    aws_instance.this,
    null_resource.overlay_net,
    null_resource.k3s_boostrap,
    null_resource.k3s_join
  ]

  content = templatefile("${path.module}/templates/setkubeconfig",{
    cluster_name = var.name,
    master_ipv4  = aws_instance.this[0].public_ip,
    ssh_key_path = "${path.module}/../artifacts/id_rsa_${var.name}"
  })
  filename        = "${path.module}/../artifacts/setkubeconfig"
  file_permission = "0755"

  provisioner "local-exec" {
    command = "${path.module}/../artifacts/setkubeconfig"
  }

}

resource "local_file" "unsetkubeconfig" {
  depends_on = [
    aws_instance.this,
    null_resource.overlay_net,
    null_resource.k3s_boostrap,
    null_resource.k3s_join
  ]

  content = templatefile("${path.module}/templates/unsetkubeconfig",{
    cluster_name = var.name
  })
  filename        = "${path.module}/../artifacts/unsetkubeconfig"
  file_permission = "0755"

  provisioner "local-exec" {
    when    = destroy
    command = "${path.module}/../artifacts/unsetkubeconfig"
  }
}
