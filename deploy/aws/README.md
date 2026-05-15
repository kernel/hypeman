# AWS Deployments

This directory contains supported AWS deployment assets for Hypeman.

## Available deployments

| Deployment | Description | Path |
| --- | --- | --- |
| Single node | One EC2 instance running Hypeman with nested virtualization enabled | [single-node](single-node) |

## Requirements

You need:

- An AWS account with permission to create EC2, IAM, CloudFormation, Lambda, Systems Manager, CloudWatch Logs, and related networking resources.
- Access to a region where the selected instance type is available.
- EC2 quota for the selected instance size.
- A VPC and subnet for the instance.
- An instance type that supports EC2 nested virtualization.

For the initial deployment path, use an Intel C8i, M8i, or R8i instance type. The default is chosen for a balance of cost and enough room to run useful Hypeman workloads.

## Launch methods

### CloudFormation

Use CloudFormation when you want a guided AWS console flow.

Start with the hosted quickstart template:

[![Launch Stack](https://s3.amazonaws.com/cloudformation-examples/cloudformation-launch-stack.png)](https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/create/review?templateURL=https%3A%2F%2Fkernel-hypeman-cloudformation-prod.s3.us-east-1.amazonaws.com%2Fv1%2Fhypeman%2Ftemplate.yaml&stackName=hypeman)

### Terraform

Use Terraform when you want to review, version, and apply the AWS resources from your existing infrastructure workflow.

[Single-node Terraform deployment](single-node#terraform)

## Networking

The single-node deployment uses an existing VPC and subnet. If public API access is enabled, restrict access to your current IP or a trusted CIDR.

The default path does not require SSH. Use AWS Systems Manager Session Manager for host access.

## Cleanup

Each deployment README includes teardown instructions. Prefer deleting the CloudFormation stack or running `terraform destroy` instead of manually deleting individual AWS resources.
