data "aws_ami" "debian" {
  most_recent = true

  filter {
    name   = "name"
    values = ["debian-11-amd64-20211220-862*"]
  }

  #  filter {
  #    name   = "virtualization-type"
  #    values = ["hvm"]
  #  }

  owners = ["aws-marketplace"]
}

output "debian_ami_name" {
  value = data.aws_ami.debian.name
}
output "debian_ami_id" {
  value = data.aws_ami.debian.id
}
output "debian_ami_desc" {
  value = data.aws_ami.debian.description
}
output "debian_ami_arch" {
  value = data.aws_ami.debian.architecture
}

## full data
#output "debian_ami" {
#  value = data.aws_ami.debian
#}
