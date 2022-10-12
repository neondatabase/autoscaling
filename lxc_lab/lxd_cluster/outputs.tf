output "lxd_public_ip" {
  value = aws_instance.this[*].public_ip
}

output "lxd_password" {
  value = random_string.password.result
}
