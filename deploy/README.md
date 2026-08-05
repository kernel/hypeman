# Deploy Hypeman

This directory contains maintained deployment assets for running Hypeman outside local development.

## AWS Quickstart

The fastest path is the hosted CloudFormation template. It creates one EC2 instance with nested virtualization enabled, installs Hypeman during instance bootstrap, exposes the Hypeman API only to the CIDR you choose, and provisions an encrypted XFS data volume mounted at `/var/lib/hypeman`.

[![Launch Stack](https://s3.amazonaws.com/cloudformation-examples/cloudformation-launch-stack.png)](https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/create/review?templateURL=https%3A%2F%2Fkernel-hypeman-cloudformation-prod.s3.us-east-1.amazonaws.com%2Fv1%2Fhypeman%2Ftemplate.yaml&stackName=hypeman)

Use `us-east-1` for the published template. Choose a VPC and subnet, set `AllowedApiCidr` to your current public IP `/32` or trusted VPN CIDR, keep SSH disabled unless you need it, then create the stack.

Useful stack outputs:

| Output | Purpose |
| --- | --- |
| `HypemanEndpoint` | Base URL for remote Hypeman API access |
| `SsmSessionCommand` | Session Manager command for host access |
| `CreateTokenCommand` | Command that generates a JWT on the instance |
| `InstanceId` | EC2 instance running Hypeman |

To delete the deployment, delete the CloudFormation stack. The EC2 instance and attached stack-managed volumes are deleted with it.

```sh
aws cloudformation delete-stack \
  --region us-east-1 \
  --stack-name hypeman
```

## Use Hypeman

After the stack reaches `CREATE_COMPLETE`, run the `SsmSessionCommand` output and generate a token:

```sh
sudo hypeman-create-token remote-user 8760h
```

On your local machine, install the CLI and point it at the `HypemanEndpoint` output:

```sh
curl -fsSL https://get.hypeman.sh/cli | bash

mkdir -p ~/.config/hypeman
cat > ~/.config/hypeman/cli.yaml <<EOF
base_url: http://<public-ip>:4973
api_key: "<jwt-from-hypeman-create-token>"
EOF

hypeman ps
```

Build, push, and run a sandbox image:

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

## CloudFormation Source

The source template lives at `deploy/aws/cloudformation/template.yaml`. The `Deploy Assets` GitHub workflow validates it on pull requests and publishes it from `main` to:

```text
https://kernel-hypeman-cloudformation-prod.s3.us-east-1.amazonaws.com/v1/hypeman/template.yaml
```

## Defaults

| Setting | Default |
| --- | --- |
| Region | `us-east-1` |
| Instance type | `c8i.2xlarge` |
| Hypeman API and internal registry port | `4973` |
| Admin access | AWS Systems Manager Session Manager |
| SSH | Disabled unless explicitly enabled |
| Root volume | 30 GiB encrypted EBS |
| Hypeman data volume | 100 GiB encrypted EBS, formatted XFS at `/var/lib/hypeman` |
| Hypeman version | `v0.3.0` |

The deployment expects an Intel C8i, M8i, or R8i instance type with EC2 nested virtualization support.
