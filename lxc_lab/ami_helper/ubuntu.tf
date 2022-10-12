#data "aws_ami" "ubuntu" {
#  most_recent = true
#
#  filter {
#    name   = "name"
#    values = ["debian-11-amd64-20211220-862*"]
#  }
#
#  #  filter {
#  #    name   = "virtualization-type"
#  #    values = ["hvm"]
#  #  }
#
#  owners = ["aws-marketplace"]
#}


data "aws_ami" "ubuntu" {
  most_recent = true

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd/ubuntu-focal-20.04-amd64-server-*"]
  }

  owners = ["099720109477"] # Canonical official
}

output "ubuntu_ami_name" {
  value = data.aws_ami.ubuntu.name
}
output "ubuntu_ami_id" {
  value = data.aws_ami.ubuntu.id
}
output "ubuntu_ami_desc" {
  value = data.aws_ami.ubuntu.description
}
output "ubuntu_ami_arch" {
  value = data.aws_ami.ubuntu.architecture
}

## full data
#output "ubuntu_ami" {
#  value = data.aws_ami.ubuntu
#}
