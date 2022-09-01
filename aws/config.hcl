locals {
  settings = read_terragrunt_config(find_in_parent_folders("settings.hcl"))
}

inputs = {
  aws_profile     = local.settings.inputs.aws_profile
  aws_region      = local.settings.inputs.aws_region
  aws_region_slug = local.settings.inputs.aws_region_slug
  name            = local.settings.inputs.name
  environment     = local.settings.inputs.environment
  vpc_cidr        = local.settings.inputs.vpc_cidr
  aws_az_count    = local.settings.inputs.aws_az_count
}

generate "provider" {
  path      = "provider.tf"
  if_exists = "overwrite_terragrunt"
  contents  = <<EOF
provider "aws" {
  region  = "${local.settings.inputs.aws_region}"
  profile = "${local.settings.inputs.aws_profile}"
}
EOF
}
