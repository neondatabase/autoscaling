# AWS AMI helper

Find AMI ID for specific os in specific AWS region

## Usage

- inspect/change [provider.tf](provider.tf) file (chenge region if need)
- run `terrafrom init`
- run `terrafrom plan`

## Example

```console
% terraform plan
data.aws_ami.ubuntu: Reading...
data.aws_ami.debian: Reading...
data.aws_ami.ubuntu: Read complete after 0s [id=ami-0b24feb030d5e3f22]
data.aws_ami.debian: Read complete after 0s [id=ami-01222432139ebc6e9]

Changes to Outputs:
  + debian_ami_arch = "x86_64"
  + debian_ami_desc = "Debian 11 (20211220-862)"
  + debian_ami_id   = "ami-01222432139ebc6e9"
  + debian_ami_name = "debian-11-amd64-20211220-862-a264997c-d509-4a51-8e85-c2644a3f8ba2"
  + ubuntu_ami_arch = "x86_64"
  + ubuntu_ami_desc = "Canonical, Ubuntu, 20.04 LTS, amd64 focal image build on 2022-10-10"
  + ubuntu_ami_id   = "ami-0b24feb030d5e3f22"
  + ubuntu_ami_name = "ubuntu/images/hvm-ssd/ubuntu-focal-20.04-amd64-server-20221010"

```

