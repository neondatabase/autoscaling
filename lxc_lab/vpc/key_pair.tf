resource "tls_private_key" "this" {
  algorithm = "RSA"
}

module "key_pair" {
  source = "terraform-aws-modules/key-pair/aws"

  key_name   = var.name
  public_key = tls_private_key.this.public_key_openssh
}

resource "local_file" "key_pair_public" {
  content              = tls_private_key.this.public_key_openssh
  filename             = "${path.module}/../artifacts/id_rsa_${var.name}.pub"
  file_permission      = "0640"
  directory_permission = "0755"
}

resource "local_file" "key_pair_private" {
  content              = tls_private_key.this.private_key_pem
  filename             = "${path.module}/../artifacts/id_rsa_${var.name}"
  file_permission      = "0600"
  directory_permission = "0755"
}
