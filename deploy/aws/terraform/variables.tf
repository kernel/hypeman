variable "region" {
  type        = string
  description = "AWS region for the Hypeman host."
  default     = "us-east-1"
}

variable "stack_name" {
  type        = string
  description = "CloudFormation stack name."
  default     = "hypeman"
}

variable "vpc_id" {
  type        = string
  description = "VPC for the Hypeman host."
}

variable "subnet_id" {
  type        = string
  description = "Subnet for the Hypeman host."
}

variable "allowed_api_cidr" {
  type        = string
  description = "CIDR allowed to reach the Hypeman API port."
}

variable "instance_type" {
  type        = string
  description = "Intel EC2 instance type that supports nested virtualization."
  default     = "c8i.2xlarge"
}

variable "root_volume_size" {
  type        = number
  description = "Root EBS volume size in GiB."
  default     = 100
}

variable "hypeman_version" {
  type        = string
  description = "Hypeman API release tag, or latest."
  default     = "latest"
}

variable "hypeman_branch" {
  type        = string
  description = "Optional Hypeman git branch to build from source. Leave empty for release install."
  default     = ""
}

variable "hypeman_cli_version" {
  type        = string
  description = "Hypeman CLI release tag, or latest."
  default     = "latest"
}

variable "enable_ssh" {
  type        = bool
  description = "Whether to open SSH."
  default     = false
}

variable "allowed_ssh_cidr" {
  type        = string
  description = "CIDR allowed to reach SSH when enabled."
  default     = "127.0.0.1/32"
}

variable "key_name" {
  type        = string
  description = "Optional EC2 key pair name for SSH."
  default     = ""
}
