#!/usr/bin/env bash
set -euo pipefail

apply=false
if [[ "${1:-}" == "--apply" ]]; then
  apply=true
elif [[ $# -gt 0 ]]; then
  echo "usage: $0 [--apply]" >&2
  exit 2
fi

region="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
stack_name="${AWS_LAMBDA_MICROVM_STACK_NAME:-elasticclaw-lambda-microvms}"
image_name="${AWS_LAMBDA_MICROVM_IMAGE_NAME:-elasticclaw-agent}"
artifact="${ELASTICCLAW_LAMBDA_MICROVM_ARTIFACT:-bin/elasticclaw-lambda-microvm.zip}"
template="aws/lambda-microvm/bootstrap.yaml"
output_config="${ELASTICCLAW_LAMBDA_MICROVM_CONFIG_OUTPUT:-bin/lambda-microvm-provider.yaml}"
hub_role_name="${AWS_LAMBDA_MICROVM_HUB_ROLE_NAME:-}"

command -v aws >/dev/null 2>&1 || { echo "aws CLI v2 is required" >&2; exit 1; }
test -f "$template" || { echo "$template is missing" >&2; exit 1; }
test -f "$artifact" || { echo "$artifact is missing; run make build-lambda-microvm-artifact" >&2; exit 1; }

export AWS_PAGER=""
aws lambda-microvms help >/dev/null
account_id="$(aws sts get-caller-identity --query Account --output text --region "$region")"
bucket_name="${AWS_LAMBDA_MICROVM_BUCKET:-elasticclaw-microvms-${account_id}-${region}}"

echo "AWS account: $account_id"
echo "Region:      $region"
echo "Stack:       $stack_name"
echo "Bucket:      $bucket_name"
echo "Image:       $image_name"

if [[ "$apply" != true ]]; then
  echo
  echo "Plan only; no AWS resources were changed."
  echo "Run 'make setup-lambda-microvm-apply' to deploy the stack, upload the artifact, and create or update the image."
  exit 0
fi

aws cloudformation deploy \
  --region "$region" \
  --stack-name "$stack_name" \
  --template-file "$template" \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
    "ArtifactBucketName=$bucket_name" \
    "ImageName=$image_name" \
  --tags elasticclaw=lambda-microvms

stack_output() {
  aws cloudformation describe-stacks \
    --region "$region" \
    --stack-name "$stack_name" \
    --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue | [0]" \
    --output text
}

build_role_arn="$(stack_output BuildRoleArn)"
runtime_policy_arn="$(stack_output HubRuntimePolicyArn)"
image_arn="$(stack_output ImageArn)"
artifact_hash="$(sha256sum "$artifact" | awk '{print $1}')"
artifact_uri="s3://${bucket_name}/artifacts/elasticclaw-lambda-microvm-${artifact_hash}.zip"

AWS_LAMBDA_MICROVM_ARTIFACT_URI="$artifact_uri" \
AWS_LAMBDA_MICROVM_BUILD_ROLE_ARN="$build_role_arn" \
AWS_LAMBDA_MICROVM_IMAGE_NAME="$image_name" \
AWS_LAMBDA_MICROVM_WAIT=1 \
AWS_REGION="$region" \
  bash scripts/publish-lambda-microvm-image.sh

if [[ -n "$hub_role_name" ]]; then
  echo "Attaching the Hub runtime policy to IAM role $hub_role_name"
  aws iam attach-role-policy \
    --role-name "$hub_role_name" \
    --policy-arn "$runtime_policy_arn"
fi

mkdir -p "$(dirname "$output_config")"
umask 077
{
  echo "providers:"
  echo "  lambda-microvms:"
  echo "    type: lambda-microvms"
  echo "    aws_region: $region"
  echo "    image_identifier: $image_arn"
  echo "    ingress_network_connectors:"
  echo "      - arn:aws:lambda:${region}:aws:network-connector:aws-network-connector:ALL_INGRESS"
  echo "    egress_network_connectors:"
  echo "      - arn:aws:lambda:${region}:aws:network-connector:aws-network-connector:INTERNET_EGRESS"
  echo "    idle_max_duration_seconds: 900"
  echo "    suspended_duration_seconds: 300"
  echo "    auto_resume: true"
  echo "    maximum_duration_seconds: 28800"
  echo "    bridge_port: 8080"
  echo "    auth_token_expiration_minutes: 30"
} > "$output_config"

echo
echo "AWS setup complete."
echo "Hub runtime policy: $runtime_policy_arn"
if [[ -z "$hub_role_name" ]]; then
  echo "Attach that policy to the IAM role used by the ElasticClaw Hub, or set AWS_LAMBDA_MICROVM_HUB_ROLE_NAME and rerun apply."
fi
echo "Merge $output_config into the Hub's protected hub.yaml, then restart the Hub."
