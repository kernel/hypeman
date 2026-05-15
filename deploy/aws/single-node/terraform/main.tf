provider "aws" {
  region = var.region
}

resource "aws_cloudformation_stack" "hypeman" {
  name          = var.stack_name
  template_body = file("${path.module}/../cloudformation/template.yaml")
  capabilities  = ["CAPABILITY_IAM"]

  parameters = {
    VpcId             = var.vpc_id
    SubnetId          = var.subnet_id
    AllowedApiCidr    = var.allowed_api_cidr
    InstanceType      = var.instance_type
    RootVolumeSize    = tostring(var.root_volume_size)
    HypemanVersion    = var.hypeman_version
    HypemanBranch     = var.hypeman_branch
    HypemanCliVersion = var.hypeman_cli_version
    EnableSSH         = tostring(var.enable_ssh)
    AllowedSshCidr    = var.allowed_ssh_cidr
    KeyName           = var.key_name
  }
}
