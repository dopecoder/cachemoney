#!/usr/bin/env bash
# nuke.sh — completely tear down EVERY AWS resource this rig created, not just the instances:
#   1. the benchmark stack (CachemoneyBenchRig) -> instances, VPC, SG, placement group, IAM
#   2. the CDK bootstrap stack (CDKToolkit) -> roles, SSM version param
#   3. the CDK assets S3 bucket (VERSIONED, Retain policy -> survives stack delete) -> emptied + removed
#   4. the CDK ECR container-assets repo (if an older bootstrap created one)
#   5. the EC2 key pair + local .pem
#
# Idempotent: safe to run repeatedly; missing resources are skipped. After this the account
# holds NOTHING from this project. Recreate from zero with:  cdk bootstrap && cdk deploy ...
#
# Usage:   ./nuke.sh                 # nukes everything in $AWS_REGION (default us-west-2)
#          KEEP_KEYPAIR=1 ./nuke.sh  # keep the key pair + .pem
set -uo pipefail

REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-west-2}}"
QUALIFIER="${CDK_QUALIFIER:-hnb659fds}" # default CDK bootstrap qualifier
RIG_STACK="${RIG_STACK:-CachemoneyBenchRig}"
KEY_NAME="${KEY_NAME:-cm-rig}"
PEM="${PEM:-$HOME/.ssh/${KEY_NAME}.pem}"
KEEP_KEYPAIR="${KEEP_KEYPAIR:-0}"

ACCOUNT="$(aws sts get-caller-identity --query Account --output text 2>/dev/null)" ||
  {
    echo "ERROR: no usable AWS credentials"
    exit 1
  }
BUCKET="cdk-${QUALIFIER}-assets-${ACCOUNT}-${REGION}"
ECR="cdk-${QUALIFIER}-container-assets-${ACCOUNT}-${REGION}"
echo ">> account=$ACCOUNT region=$REGION"

del_stack() {
  local s="$1"
  if aws cloudformation describe-stacks --stack-name "$s" --region "$REGION" >/dev/null 2>&1; then
    echo ">> deleting stack $s ..."
    aws cloudformation delete-stack --stack-name "$s" --region "$REGION"
    aws cloudformation wait stack-delete-complete --stack-name "$s" --region "$REGION" 2>/dev/null
    echo "   stack $s gone"
  else
    echo ">> stack $s already absent"
  fi
}

empty_and_remove_bucket() {
  local b="$1"
  aws s3api head-bucket --bucket "$b" --region "$REGION" >/dev/null 2>&1 ||
    {
      echo ">> bucket $b absent"
      return
    }
  echo ">> emptying versioned bucket $b ..."
  for _ in 1 2 3 4 5 6 7 8; do
    local js
    js="$(aws s3api list-object-versions --bucket "$b" --region "$REGION" --max-items 1000 \
      --query '{Objects: [Versions[].{Key:Key,VersionId:VersionId}, DeleteMarkers[].{Key:Key,VersionId:VersionId}][]}' \
      --output json 2>/dev/null)"
    echo "$js" | grep -q '"Key"' || break
    echo "$js" >/tmp/nuke-del.json
    aws s3api delete-objects --bucket "$b" --region "$REGION" --delete file:///tmp/nuke-del.json >/dev/null 2>&1
  done
  aws s3 rb "s3://$b" --region "$REGION" >/dev/null 2>&1 &&
    echo "   bucket $b removed" || echo "   WARN: bucket $b not removed (check manually)"
}

# 1+2: stacks (CDKToolkit retains the bucket, so the bucket is handled separately below)
del_stack "$RIG_STACK"
del_stack "CDKToolkit"

# 3: the orphaned, versioned assets bucket
empty_and_remove_bucket "$BUCKET"

# 4: ECR container-assets repo (only older bootstraps create it)
if aws ecr delete-repository --repository-name "$ECR" --region "$REGION" --force >/dev/null 2>&1; then
  echo ">> ECR repo $ECR deleted"
else
  echo ">> ECR repo $ECR absent"
fi

# 5: bootstrap version SSM param (usually removed with the stack; belt-and-suspenders)
aws ssm delete-parameter --name "/cdk-bootstrap/${QUALIFIER}/version" --region "$REGION" >/dev/null 2>&1 &&
  echo ">> SSM /cdk-bootstrap/${QUALIFIER}/version deleted" || true

# 6: key pair + local pem
if [ "$KEEP_KEYPAIR" != "1" ]; then
  aws ec2 delete-key-pair --key-name "$KEY_NAME" --region "$REGION" >/dev/null 2>&1 &&
    echo ">> key pair $KEY_NAME deleted" || echo ">> key pair $KEY_NAME absent"
  [ -f "$PEM" ] && rm -f "$PEM" && echo ">> removed local $PEM"
fi

echo ">> NUKE COMPLETE — account holds nothing from this rig in $REGION."
