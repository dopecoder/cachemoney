"""cachemoney benchmark rig — CDK app entry point.

Required context:
  -c key_name=<existing EC2 key pair name>
  -c allowed_ssh_cidr=<cidr>   (SSH ingress; fail-closed. Lock to your IP: $(curl -s ifconfig.me)/32)

Optional context (smoke tiny-first, then scale up to the metal pair):
  -c server_instance_type=r8g.metal-24xl   (default: t4g.medium  — tiny ARM for smoke)
  -c client_instance_type=c8g.24xlarge     (default: t4g.small   — tiny ARM for smoke)
  -c volume_gb=80                          (default: 30)
"""

import os

import aws_cdk as cdk

from rig.rig_stack import RigStack

app = cdk.App()


def ctx(key: str, default: str) -> str:
    value = app.node.try_get_context(key)
    return str(value) if value not in (None, "") else default


key_name = app.node.try_get_context("key_name")
if not key_name:
    raise SystemExit(
        "Missing required context: -c key_name=<your-ec2-keypair-name>\n"
        "Create one with:  aws ec2 create-key-pair --key-name cm-rig "
        "--query KeyMaterial --output text > ~/.ssh/cm-rig.pem && chmod 600 ~/.ssh/cm-rig.pem"
    )

# Fail closed: never default SSH ingress to the whole internet. The boxes have public IPs
# (and an SSM role), so an explicit CIDR is mandatory. Pass 0.0.0.0/0 only if you truly mean it.
allowed_ssh_cidr = app.node.try_get_context("allowed_ssh_cidr")
if not allowed_ssh_cidr:
    raise SystemExit(
        "Missing required context: -c allowed_ssh_cidr=<cidr>\n"
        "Lock SSH to your IP:  -c allowed_ssh_cidr=$(curl -s ifconfig.me)/32"
    )

RigStack(
    app,
    "CachemoneyBenchRig",
    server_instance_type=ctx("server_instance_type", "t4g.medium"),
    client_instance_type=ctx("client_instance_type", "t4g.small"),
    key_name=key_name,
    allowed_ssh_cidr=str(allowed_ssh_cidr),
    volume_gb=int(ctx("volume_gb", "30")),
    # Cluster placement group: required for the metal run, unsupported on burstable t4g.*
    # smoke instances. Pass -c placement_group=false for the tiny smoke.
    use_placement_group=ctx("placement_group", "true").lower()
    not in ("false", "0", "no"),
    env=cdk.Environment(
        account=os.environ.get("CDK_DEFAULT_ACCOUNT"),
        region=os.environ.get("CDK_DEFAULT_REGION"),
    ),
)

app.synth()
