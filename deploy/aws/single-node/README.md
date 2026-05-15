# AWS Single-Node Deployment

This deployment creates one AWS EC2 instance running Hypeman with nested virtualization enabled.

It is intended for quick evaluation, development, and small internal deployments. It is not a high-availability deployment.

## What it creates

Depending on the launch method, this deployment creates or configures:

- One EC2 instance using a supported Intel instance type.
- Nested virtualization enabled for the instance.
- An IAM role for Systems Manager access and basic instance operation.
- A security group that exposes the Hypeman API port only to the configured CIDR.
- Optional SSH access.
- Encrypted EBS storage.
- Instance bootstrap logs and Lambda custom-resource logs for launch troubleshooting.
- A Hypeman install controlled by version or branch parameters.
- Stack or Terraform outputs with connection and health-check commands.

## Defaults

| Setting | Default |
| --- | --- |
| Region | `us-east-1` |
| Instance type | `c8i.2xlarge` |
| Hypeman API port | `8080` |
| Admin access | AWS Systems Manager Session Manager |
| SSH | Disabled unless explicitly enabled |
| Root volume | 100 GiB encrypted EBS |
| Hypeman version | Latest release with a matching artifact |

## CloudFormation

Use this path for the AWS quickstart. The launch link opens the hosted CloudFormation template published from the `main` branch.

[Launch Hypeman on AWS](https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/create/review?templateURL=https%3A%2F%2Fkernel-hypeman-cloudformation-prod.s3.us-east-1.amazonaws.com%2Fv1%2Fhypeman%2Ftemplate.yaml&stackName=hypeman)

Review these parameters before creating the stack:

1. Choose a VPC and subnet.
2. Choose an instance type. Start with `c8i.2xlarge` unless you know you need more memory or CPU.
3. Set `AllowedApiCidr` to your current IP range or a trusted VPN CIDR.
4. Leave SSH disabled unless you specifically need it.
5. Create the stack.
6. Wait for `CREATE_COMPLETE`.
7. Open the stack outputs.

Useful outputs include:

| Output | Purpose |
| --- | --- |
| `InstanceId` | EC2 instance running Hypeman |
| `PublicIp` | Public IP for the Hypeman API, if the subnet assigns one |
| `HypemanEndpoint` | Base URL for remote Hypeman API access |
| `SsmSessionCommand` | Command to open a shell through Session Manager |
| `CreateTokenCommand` | Command to generate a JWT on the instance |

To delete the deployment:

```sh
aws cloudformation delete-stack \
  --region us-east-1 \
  --stack-name hypeman
```

## Terraform

Use this path if you manage AWS infrastructure with Terraform.

```sh
cd deploy/aws/single-node/terraform
terraform init
terraform apply \
  -var="region=us-east-1" \
  -var="vpc_id=vpc-..." \
  -var="subnet_id=subnet-..." \
  -var="allowed_api_cidr=$(curl -fsSL https://checkip.amazonaws.com)/32" \
  -var="instance_type=c8i.2xlarge"
```

After apply completes, inspect the outputs:

```sh
terraform output
```

To delete the deployment:

```sh
terraform destroy
```

## Use Hypeman

First generate a JWT on the Hypeman host. Use the `SsmSessionCommand` output to connect, then run:

```sh
sudo hypeman-create-token remote-user 8760h
```

On your local machine, install the Hypeman CLI and configure it to use the remote API:

```sh
curl -fsSL https://get.hypeman.sh/cli | bash

mkdir -p ~/.config/hypeman
cat > ~/.config/hypeman/cli.yaml <<EOF
base_url: http://<public-ip>:8080
api_key: "<jwt-from-hypeman-create-token>"
EOF

hypeman ps
```

The security group must allow your current IP or VPN CIDR to reach port `8080`.

### Push and run a sandbox image

This example builds a small image locally, pushes it to the Hypeman host with the Hypeman CLI, and runs it as a sandboxed workload.

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
hypeman exec -it claude-code-sandbox bash
```

When you are done:

```sh
hypeman stop claude-code-sandbox
hypeman rm claude-code-sandbox
```

## Host validation

To validate the host itself, connect through Session Manager and run:

```sh
sudo /opt/hypeman/deploy/validate.sh
```

The validation script checks nested virtualization, the Hypeman service, and local API reachability.

## Troubleshooting

### Nested virtualization is unavailable

Confirm the instance type is one of the supported Intel families and that the deployment enabled nested virtualization at launch.

### Instance launch fails

Check EC2 service quotas for the selected instance family and region. Try `c8i.2xlarge` first, then move to larger sizes after the basic launch works.

### Session Manager does not connect

Confirm the instance role was created, the SSM agent is running, and the subnet has outbound access to AWS Systems Manager endpoints.

### Remote API access fails

Confirm the stack output endpoint uses the current public IP and that `AllowedApiCidr` includes your client IP. Then check the service locally from the instance:

```sh
curl -fsS http://127.0.0.1:8080/health
```

### Hypeman is not healthy

Check setup and service logs:

```sh
sudo journalctl -u hypeman --no-pager -n 200
sudo cat /var/log/hypeman-bootstrap.log
```

## Production notes

For longer-lived deployments:

- Use an existing VPC and a subnet with controlled routing.
- Keep admin access behind Session Manager or a VPN.
- Pin the Hypeman version.
- Enable CloudWatch alarms.
- Snapshot or retain the data volume before deleting the stack.
- Review security group rules before enabling public API access.
