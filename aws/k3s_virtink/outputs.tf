output "k3s_public_ip" {
  value = aws_instance.this[*].public_ip
}

#output "efs_id" {
#  value = aws_efs_file_system.this.id
#}
