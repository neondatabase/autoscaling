inputs = {
  aws_profile     = "neon"
  aws_region      = "eu-west-1"
  aws_region_slug = "ireland"

  aws_az_count = 3

  name        = "lm-lab"
  environment = "dev"

  vpc_cidr = "10.1.0.0/16"
}
