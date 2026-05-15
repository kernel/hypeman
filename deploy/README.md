# Deploy Hypeman

This directory contains supported deployment assets for running Hypeman outside local development.

## Quickstart: AWS CloudFormation

The first-class AWS quickstart is the single-node CloudFormation deployment. It launches one EC2 host with nested virtualization enabled, exposes the Hypeman API only to the CIDR you choose, and prints the commands needed to connect and create a JWT.

Open the hosted CloudFormation template in `us-east-1`:

[Launch Hypeman on AWS](https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/create/review?templateURL=https%3A%2F%2Fkernel-hypeman-cloudformation-prod.s3.us-east-1.amazonaws.com%2Fv1%2Fhypeman%2Ftemplate.yaml&stackName=hypeman)

Choose a VPC and subnet, set `AllowedApiCidr` to your current IP range or trusted VPN CIDR, then create the stack.

After the stack reaches `CREATE_COMPLETE`, use the `SsmSessionCommand` output to open a Session Manager shell and generate a remote API token:

```sh
sudo hypeman-create-token remote-user 8760h
```

On your local machine, install the Hypeman CLI and point it at the stack's `HypemanEndpoint` output:

```sh
curl -fsSL https://get.hypeman.sh/cli | bash

mkdir -p ~/.config/hypeman
cat > ~/.config/hypeman/cli.yaml <<EOF
base_url: http://<public-ip>:8080
api_key: "<jwt-from-hypeman-create-token>"
EOF

hypeman ps
```

Then push and run a real sandbox image through the remote API:

```sh
mkdir -p /tmp/hypeman-claude-code
cat > /tmp/hypeman-claude-code/Dockerfile <<'EOF'
FROM node:22-bookworm-slim
RUN npm install -g @anthropic-ai/claude-code
WORKDIR /workspace
CMD ["sleep", "infinity"]
EOF

docker build -t local/claude-code-sandbox:latest /tmp/hypeman-claude-code
hypeman push local/claude-code-sandbox:latest sandbox/claude-code:latest

until hypeman image get sandbox/claude-code:latest | grep -qi ready; do
  sleep 2
done

hypeman run --name claude-code-sandbox sandbox/claude-code:latest
hypeman exec claude-code-sandbox -- claude --version
```

Clean up the sandbox when you are done:

```sh
hypeman stop claude-code-sandbox
hypeman rm claude-code-sandbox
```

See the [AWS single-node guide](aws/single-node) for CloudFormation parameters, Terraform usage, troubleshooting, and teardown.

## Supported deployments

| Platform | Deployment | Best for | Path |
| --- | --- | --- | --- |
| AWS | Single node | Trying Hypeman quickly, small internal deployments, development hosts | [aws/single-node](aws/single-node) |

## Choosing a deployment path

Use the AWS single-node deployment if you want the fastest path to a working Hypeman host in your own AWS account.

The single-node deployment provides two launch surfaces:

| Method | Best for |
| --- | --- |
| CloudFormation | Click-through setup in the AWS console |
| Terraform | Teams that manage AWS infrastructure with Terraform |

All methods create the same basic shape: one EC2 instance with nested virtualization enabled, an instance role, security group rules, encrypted storage, logging, and startup automation for Hypeman.

## Security model

The deployment defaults are intentionally conservative:

- Administration uses AWS Systems Manager Session Manager by default.
- SSH is optional.
- Inbound access is restricted by CIDR parameters.
- EBS volumes are encrypted.
- The Hypeman version is controlled by parameter.
- Stack deletion removes created resources unless data retention is explicitly enabled.

Review the cloud-specific README before launching anything in a production AWS account.

## Cost

Cloud resources created from these templates bill to your cloud account. The largest cost is the EC2 instance. Stop or delete the deployment when you are done testing.

## Support level

Files under this directory are intended to be maintained deployment paths, not throwaway examples. Changes should preserve upgrade, teardown, and security behavior unless the README explicitly calls out a breaking change.
