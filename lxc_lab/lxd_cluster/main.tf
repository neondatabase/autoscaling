resource "aws_instance" "this" {
  count = var.cluster_size

  ami           = var.ami_id
  instance_type = var.ec2_type
  key_name      = var.key_name
  ebs_optimized = true

  iam_instance_profile = aws_iam_instance_profile.this.id

  user_data_base64 = base64encode(local.user_data)

  #subnet_id = var.public_subnets[0] # place instances in one subnet
  subnet_id = var.public_subnets[count.index] # spread instances over subnets

  # ensure we do setup public IP
  associate_public_ip_address = true

  vpc_security_group_ids = [module.security_group.security_group_id]

  monitoring = false

  root_block_device {
    volume_size = 100 # GiB
    #    volume_type = "gp3"
    #    iops        = 3000
    #    throughput  = 500 # MiB/s
  }

  tags = {
    Name = "${var.name}-${count.index}"
  }
}
