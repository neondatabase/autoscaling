### Filled by terragrunt

variable "name" {
  type        = string
  description = "Common name for all resources"
}

variable "vpc_cidr" {
  type        = string
  description = "IPv4 IP block for VPC"
}

variable "aws_az_count" {
  type        = number
  description = "count of AZs to use"
}
