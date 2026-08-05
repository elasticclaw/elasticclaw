# AWS Lambda MicroVM provider

This provider runs each ElasticClaw agent in a stateful, Firecracker-isolated AWS Lambda MicroVM. The image contains two processes:

- `lambda-microvm-bridge` is snapshotted into the image, handles AWS lifecycle hooks and exposes the authenticated HTTPS control API on port 8080.
- `claw-bridge` starts only after the Hub sends per-claw environment and workspace files through that control API. Secrets are not baked into the shared image or the AWS run-hook payload.

The provider is alpha. Lambda MicroVMs currently supports sessions up to eight hours, and using the commands below creates billable AWS resources.

## Prerequisites

- AWS CLI v2 with the `lambda-microvms` command (v2.36 or newer).
- AWS access to deploy CloudFormation/IAM/S3 resources, pass the generated build role to Lambda, and create or update a MicroVM image. The examples use `us-east-1`.
- Hub credentials from the standard AWS credential chain. Prefer an IAM role. For local development, use a named profile or short-lived `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional `AWS_SESSION_TOKEN` values.

The Hub operator needs these actions for runtime operation:

```json
[
  "lambda:RunMicrovm",
  "lambda:GetMicrovm",
  "lambda:ListMicrovms",
  "lambda:SuspendMicrovm",
  "lambda:ResumeMicrovm",
  "lambda:TerminateMicrovm",
  "lambda:CreateMicrovmAuthToken"
]
```

If an execution role is configured for agents, also allow the Hub identity to pass only that role with `iam:PassRole`.

## One-time AWS setup

Use AWS IAM Identity Center (SSO) for developers instead of distributing access keys:

```bash
aws configure sso --profile elasticclaw-dev
aws sso login --profile elasticclaw-dev
export AWS_PROFILE=elasticclaw-dev
export AWS_REGION=us-east-1
```

Copy the environment template and inspect the read-only plan. The plan resolves your AWS account and prints deterministic resource names but does not create anything:

```bash
cp aws/lambda-microvm/aws.env.example aws/lambda-microvm/.env
set -a
. aws/lambda-microvm/.env
set +a
make setup-lambda-microvm
```

An AWS platform administrator or deployment role runs the apply target once:

```bash
make setup-lambda-microvm-apply
```

This deploys `bootstrap.yaml`, uploads the content-addressed image artifact, creates or updates the MicroVM image, waits for it to become `CREATED`, and writes an untracked provider fragment to `bin/lambda-microvm-provider.yaml`. The CloudFormation stack owns:

- an encrypted, versioned, private S3 artifact bucket;
- the Lambda-assumable image build role;
- a least-privilege managed policy for Hub runtime operations.

Set `AWS_LAMBDA_MICROVM_HUB_ROLE_NAME` before applying to attach that managed policy to an existing Hub role automatically. If the Hub runs outside AWS, leave it blank and arrange workload federation or an AWS profile separately. The script never writes AWS credentials to repository files.

## Build and publish the image

The one-time setup above also publishes the initial image. For later image-only releases, fill in the artifact S3 URI and build-role ARN in the ignored `.env`, then run:

```bash
cp aws/lambda-microvm/aws.env.example aws/lambda-microvm/.env
set -a
. aws/lambda-microvm/.env
set +a

make publish-lambda-microvm-image
```

The publish command uploads `bin/elasticclaw-lambda-microvm.zip` and starts the asynchronous image build with the lifecycle hooks required by ElasticClaw. It does not wait for completion. Check it with:

```bash
aws lambda-microvms get-microvm-image \
  --region "$AWS_REGION" \
  --image-identifier "$AWS_LAMBDA_MICROVM_IMAGE_NAME"
```

When the state becomes `CREATED`, copy the returned `imageArn` and `imageVersion` into the Hub settings UI or merge [hub.yaml.example](hub.yaml.example) into `~/.elasticclaw/hub.yaml`. The Hub process must be able to execute the same AWS CLI and access the same credential chain.

## Credentials

No new ElasticClaw-specific secret is required. Do not put AWS access keys in `hub.yaml`. Use one of:

- an IAM role attached to the Hub workload (recommended);
- `AWS_PROFILE` plus the normal AWS shared config files;
- short-lived AWS environment credentials.

The `.env` file in this directory is ignored by Git.
