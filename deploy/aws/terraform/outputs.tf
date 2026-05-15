output "instance_id" {
  description = "EC2 instance running Hypeman."
  value       = aws_cloudformation_stack.hypeman.outputs["InstanceId"]
}

output "public_ip" {
  description = "Public IP address, if assigned."
  value       = aws_cloudformation_stack.hypeman.outputs["PublicIp"]
}

output "private_ip" {
  description = "Private IP address."
  value       = aws_cloudformation_stack.hypeman.outputs["PrivateIp"]
}

output "hypeman_endpoint" {
  description = "Hypeman API endpoint."
  value       = aws_cloudformation_stack.hypeman.outputs["HypemanEndpoint"]
}

output "ssm_session_command" {
  description = "Command to start a Session Manager shell."
  value       = aws_cloudformation_stack.hypeman.outputs["SsmSessionCommand"]
}

output "create_token_command" {
  description = "Command to generate a JWT on the instance."
  value       = aws_cloudformation_stack.hypeman.outputs["CreateTokenCommand"]
}
