data "aws_availability_zones" "available" {
  state = "available"
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "3.14.2"

  name = var.name
  cidr = var.vpc_cidr

  # use aws_az_count azs from list
  azs = slice(data.aws_availability_zones.available.names, 0, var.aws_az_count)

  public_subnets = [for k, v in slice(data.aws_availability_zones.available.names, 0, var.aws_az_count) : cidrsubnet(var.vpc_cidr, 8, k)]

  # single NAT Gateway
  #  enable_nat_gateway     = true
  #  single_nat_gateway     = true
  #  one_nat_gateway_per_az = false

  enable_dns_hostnames = true
  enable_dns_support   = true
}
