#!/usr/bin/env bash
set -euo pipefail

artifact="${ELASTICCLAW_LAMBDA_MICROVM_ARTIFACT:-bin/elasticclaw-lambda-microvm.zip}"
region="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
artifact_uri="${AWS_LAMBDA_MICROVM_ARTIFACT_URI:-}"
build_role_arn="${AWS_LAMBDA_MICROVM_BUILD_ROLE_ARN:-}"
image_name="${AWS_LAMBDA_MICROVM_IMAGE_NAME:-elasticclaw-agent}"
base_image_arn="${AWS_LAMBDA_MICROVM_BASE_IMAGE_ARN:-arn:aws:lambda:${region}:aws:microvm-image:al2023-1}"
wait_for_image="${AWS_LAMBDA_MICROVM_WAIT:-0}"

command -v aws >/dev/null 2>&1 || { echo "aws is required" >&2; exit 1; }
test -f "$artifact" || { echo "$artifact is missing; run make build-lambda-microvm-artifact" >&2; exit 1; }
test -n "$artifact_uri" || { echo "AWS_LAMBDA_MICROVM_ARTIFACT_URI is required" >&2; exit 1; }
test -n "$build_role_arn" || { echo "AWS_LAMBDA_MICROVM_BUILD_ROLE_ARN is required" >&2; exit 1; }

export AWS_PAGER=""
aws lambda-microvms help >/dev/null
caller_arn="$(aws sts get-caller-identity --region "$region" --query Arn --output text)"
account_id="$(aws sts get-caller-identity --region "$region" --query Account --output text)"
partition="${caller_arn#arn:}"
partition="${partition%%:*}"
image_identifier="arn:${partition}:lambda:${region}:${account_id}:microvm-image:${image_name}"
aws s3 cp "$artifact" "$artifact_uri" --region "$region"
hooks='{"port":8080,"microvmHooks":{"run":"ENABLED","runTimeoutInSeconds":60,"resume":"ENABLED","resumeTimeoutInSeconds":30,"suspend":"ENABLED","suspendTimeoutInSeconds":30,"terminate":"ENABLED","terminateTimeoutInSeconds":30},"microvmImageHooks":{"ready":"ENABLED","readyTimeoutInSeconds":300,"validate":"ENABLED","validateTimeoutInSeconds":300}}'
client_token="elasticclaw-$(sha256sum "$artifact" | awk '{print $1}')"

common_args=(
  --region "$region"
  --code-artifact "uri=$artifact_uri"
  --base-image-arn "$base_image_arn"
  --build-role-arn "$build_role_arn"
  --hooks "$hooks"
  --client-token "$client_token"
  --output json
)

get_error="$(mktemp)"
trap 'rm -f "$get_error"' EXIT
if aws lambda-microvms get-microvm-image --region "$region" --image-identifier "$image_identifier" >/dev/null 2>"$get_error"; then
  echo "Updating Lambda MicroVM image $image_name"
  aws lambda-microvms update-microvm-image \
    --image-identifier "$image_identifier" \
    "${common_args[@]}"
elif grep -Eqi 'ResourceNotFound|not found|does not exist' "$get_error"; then
  echo "Creating Lambda MicroVM image $image_name"
  aws lambda-microvms create-microvm-image \
    --name "$image_name" \
    --tags 'elasticclaw=agent-provider' \
    "${common_args[@]}"
else
  echo "Unable to check whether Lambda MicroVM image $image_name exists:" >&2
  sed 's/^/  /' "$get_error" >&2
  exit 1
fi

if [[ "$wait_for_image" == "1" ]]; then
  echo "Waiting for Lambda MicroVM image build..."
  for attempt in $(seq 1 120); do
    state="$(aws lambda-microvms get-microvm-image --region "$region" --image-identifier "$image_identifier" --query state --output text)"
    case "$state" in
      CREATED|UPDATED)
        aws lambda-microvms get-microvm-image --region "$region" --image-identifier "$image_identifier" --output json
        exit 0
        ;;
      CREATE_FAILED|UPDATE_FAILED)
        echo "Lambda MicroVM image build failed; inspect /aws/lambda/microvms/$image_name in CloudWatch Logs" >&2
        exit 1
        ;;
    esac
    sleep 10
  done
  echo "Timed out waiting for Lambda MicroVM image $image_name" >&2
  exit 1
fi
