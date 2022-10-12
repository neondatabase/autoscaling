include "common" {
  path = "${get_terragrunt_dir()}/../config.hcl"
}

dependency "vpc" {
  config_path = "${get_terragrunt_dir()}/../vpc"

  mock_outputs = {
    vpc_id          = "vpc-dummy-id"
    public_subnets  = ["public-subnet1-id", "public-subnet2-id", "public-subnet3-id"]
    key_name        = "ssh-key"
    ssh_private_key = "key_in_pem_format"
  }
  mock_outputs_allowed_terraform_commands = ["validate"]
}

inputs = {
  vpc_id          = dependency.vpc.outputs.vpc_id
  public_subnets  = dependency.vpc.outputs.public_subnets
  key_name        = dependency.vpc.outputs.key_name
  ssh_private_key = dependency.vpc.outputs.ssh_private_key
}
