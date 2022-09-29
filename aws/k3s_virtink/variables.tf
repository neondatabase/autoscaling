variable "cluster_size" {
  type        = number
  description = "K3S cluster size"
  default     = 3
}

# metal instances with SSD/NVMe disks
#
# type		price		CPU	Mem	Storage				Net	EBS net
# z1d.metal	4,464 USD	48	384	2 x 900 SSD на базе NVMe	25
# m6id.metal	7,5936 USD	128	512	4 x 1900 NVMe SSD		50	40
# m5d.metal	5,424 USD	96	384	4 x 900 SSD на базе NVMe	25	19
# m5dn.metal	6,528 USD	96	384	4 x 900 SSD на базе NVMe	100	19
# c6id.metal	6,4512 USD	128	256	4 x 1900 NVMe SSD		50	40
# c5d.metal	4,608 USD	96	192	4 x 900 SSD на базе NVMe	25	19
# i4i.metal	10,982 USD	128	1024	8 x 3750 SSD на базе AWS Nitro	75	40
# i3.metal	4,992 USD	72	512	8 × 1900 SSD на базе NVMe	25
# i3en.metal	10,848 USD	96	768	8 x 7500 SSD на базе NVMe	100

variable "ec2_type" {
  type        = string
  description = "EC2 instance type for K3S cluster nodes"
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
