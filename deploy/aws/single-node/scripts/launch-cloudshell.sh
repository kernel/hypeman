#!/usr/bin/env bash
set -euo pipefail

region="${HYPEMAN_AWS_REGION:-${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}}"
stack_name="${HYPEMAN_STACK_NAME:-hypeman-single-node}"
instance_type="${HYPEMAN_INSTANCE_TYPE:-c8i.2xlarge}"
root_volume_size="${HYPEMAN_ROOT_VOLUME_SIZE:-100}"
hypeman_version="${HYPEMAN_VERSION:-latest}"
hypeman_branch="${HYPEMAN_BRANCH:-}"
hypeman_cli_version="${HYPEMAN_CLI_VERSION:-latest}"
template_url="${HYPEMAN_TEMPLATE_URL:-https://raw.githubusercontent.com/kernel/hypeman/main/deploy/aws/single-node/cloudformation/template.yaml}"

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

template_file="$tmp_dir/template.yaml"
curl -fsSL "$template_url" -o "$template_file"

vpc_id="${HYPEMAN_VPC_ID:-}"
if [ -z "$vpc_id" ]; then
  vpc_id="$(aws ec2 describe-vpcs \
    --region "$region" \
    --filters Name=is-default,Values=true \
    --query 'Vpcs[0].VpcId' \
    --output text)"
fi
if [ -z "$vpc_id" ] || [ "$vpc_id" = "None" ]; then
  echo "Set HYPEMAN_VPC_ID; no default VPC was found in $region." >&2
  exit 1
fi

subnet_id="${HYPEMAN_SUBNET_ID:-}"
if [ -z "$subnet_id" ]; then
  subnet_id="$(aws ec2 describe-subnets \
    --region "$region" \
    --filters "Name=vpc-id,Values=$vpc_id" Name=default-for-az,Values=true \
    --query 'Subnets[0].SubnetId' \
    --output text)"
fi
if [ -z "$subnet_id" ] || [ "$subnet_id" = "None" ]; then
  echo "Set HYPEMAN_SUBNET_ID; no default subnet was found in $vpc_id." >&2
  exit 1
fi

allowed_api_cidr="${HYPEMAN_ALLOWED_API_CIDR:-}"
if [ -z "$allowed_api_cidr" ]; then
  allowed_api_cidr="$(curl -fsSL https://checkip.amazonaws.com | tr -d '[:space:]')/32"
fi

params=(
  "VpcId=$vpc_id"
  "SubnetId=$subnet_id"
  "AllowedApiCidr=$allowed_api_cidr"
  "InstanceType=$instance_type"
  "RootVolumeSize=$root_volume_size"
  "HypemanVersion=$hypeman_version"
  "HypemanBranch=$hypeman_branch"
  "HypemanCliVersion=$hypeman_cli_version"
)

if [ "${HYPEMAN_ENABLE_SSH:-false}" = "true" ]; then
  params+=("EnableSSH=true")
  params+=("AllowedSshCidr=${HYPEMAN_ALLOWED_SSH_CIDR:-$allowed_api_cidr}")
  if [ -n "${HYPEMAN_KEY_NAME:-}" ]; then
    params+=("KeyName=$HYPEMAN_KEY_NAME")
  fi
fi

aws cloudformation deploy \
  --region "$region" \
  --stack-name "$stack_name" \
  --template-file "$template_file" \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides "${params[@]}"

aws cloudformation describe-stacks \
  --region "$region" \
  --stack-name "$stack_name" \
  --query 'Stacks[0].Outputs[*].[OutputKey,OutputValue]' \
  --output table
