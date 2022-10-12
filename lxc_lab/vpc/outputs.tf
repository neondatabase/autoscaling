output "vpc_id" {
  value = module.vpc.vpc_id
}
output "public_subnets" {
  value = module.vpc.public_subnets
}
output "key_name" {
  value = var.name
}
output "ssh_private_key" {
  value     = tls_private_key.this.private_key_pem
  sensitive = true
}
