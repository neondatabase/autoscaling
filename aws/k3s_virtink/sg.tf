module "security_group" {
  source  = "terraform-aws-modules/security-group/aws"
  version = "~> 4.0"

  name        = var.name
  description = "${var.name} security group"
  vpc_id      = var.vpc_id

  #ingress_cidr_blocks = ["0.0.0.0/0"]
  #ingress_rules = ["all-all"]

  egress_cidr_blocks = ["0.0.0.0/0"]
  egress_rules       = ["all-all"]

  ingress_with_cidr_blocks = [
    {
      description = "VPC full access"
      rule        = "all-all"
      cidr_blocks = var.vpc_cidr
    },
    {
      description = "SSH external access"
      rule        = "ssh-tcp"
      cidr_blocks = "0.0.0.0/0"
    },
    {
      description = "K3S API external access"
      from_port   = 6443
      to_port     = 6443
      protocol    = "tcp"
      cidr_blocks = "0.0.0.0/0"
    },
  ]
}
