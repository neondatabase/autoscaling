variable "cluster_size" {
  type        = number
  description = "LXD cluster size"
  default     = 3
}

variable "ec2_type" {
  type        = string
  description = "EC2 instance type for LXD cluster nodes"
  default     = "c5n.metal"
  #default     = "m6i.large"
}

variable "ami_id" {
  type        = string
  description = "AMI_ID for latest Ubuntu 20.04"
  default     = "ami-07702eb3b2ef420a9" # "Canonical, Ubuntu, 20.04 LTS, amd64 focal image build on 2022-08-10"
}

### Filled by terragrunt

variable "name" {
  type        = string
  description = "Common name for all resources"
}

variable "environment" {
  type        = string
  description = "Environment name"
}

variable "vpc_id" {
  type        = string
  description = "VPC ID"
}

variable "public_subnets" {
  type        = list(string)
  default     = []
  description = "List of VPC subnet IDs"
}

variable "aws_profile" {
  type        = string
  description = "Name of AWS Profile"
}

variable "aws_region" {
  type        = string
  description = "Name of AWS Region"
}

variable "aws_region_slug" {
  type        = string
  description = "Slug name of AWS Region"
}

variable "key_name" {
  type        = string
  description = "ssh keypair name to access eks nodes"
}

variable "ssh_private_key" {
  type = string
}

variable "vpc_cidr" {
  type        = string
  description = "IPv4 IP block for VPC"
}
