"""CDK stack for the cachemoney two-machine benchmark rig.

A single-AZ VPC, a cluster placement group (max inter-node bandwidth / min latency), one
security group that allows ALL traffic between the two instances plus SSH from the operator,
and two Graviton (arm64) instances: a server-under-test and a load generator. Instance types
are parameters so you smoke on small instances first, then run on the metal pair.
"""

from pathlib import Path

from aws_cdk import (
    CfnOutput,
    Stack,
    Tags,
    aws_ec2 as ec2,
    aws_iam as iam,
)
from constructs import Construct

PROVISION_DIR = Path(__file__).resolve().parent.parent / "provision"


class RigStack(Stack):
    def __init__(
        self,
        scope: Construct,
        construct_id: str,
        *,
        server_instance_type: str,
        client_instance_type: str,
        key_name: str,
        allowed_ssh_cidr: str,
        volume_gb: int,
        **kwargs,
    ) -> None:
        super().__init__(scope, construct_id, **kwargs)

        vpc = ec2.Vpc(
            self,
            "Vpc",
            max_azs=1,
            nat_gateways=0,
            ip_addresses=ec2.IpAddresses.cidr("10.0.0.0/16"),
            subnet_configuration=[
                ec2.SubnetConfiguration(
                    name="public",
                    subnet_type=ec2.SubnetType.PUBLIC,
                    cidr_mask=24,
                )
            ],
        )

        # Cluster strategy keeps both nodes on the same low-latency, high-bandwidth segment.
        placement_group = ec2.PlacementGroup(
            self,
            "Pg",
            strategy=ec2.PlacementGroupStrategy.CLUSTER,
        )

        sg = ec2.SecurityGroup(
            self,
            "Sg",
            vpc=vpc,
            description="cachemoney bench rig",
            allow_all_outbound=True,
        )
        # All traffic between the two rig instances (self-referencing rule) — every DB port,
        # SSH control-plane, and the load traffic flow over the private network.
        sg.add_ingress_rule(sg, ec2.Port.all_traffic(), "all traffic within the rig")
        # SSH from the operator's network (override with -c allowed_ssh_cidr=<your-ip>/32).
        sg.add_ingress_rule(
            ec2.Peer.ipv4(allowed_ssh_cidr), ec2.Port.tcp(22), "operator ssh"
        )

        ami = ec2.MachineImage.latest_amazon_linux2023(
            cpu_type=ec2.AmazonLinuxCpuType.ARM_64,
        )
        key = ec2.KeyPair.from_key_pair_name(self, "Key", key_name)
        role = iam.Role(
            self,
            "Role",
            assumed_by=iam.ServicePrincipal("ec2.amazonaws.com"),
            managed_policies=[
                iam.ManagedPolicy.from_aws_managed_policy_name(
                    "AmazonSSMManagedInstanceCore"
                ),
            ],
        )
        block_devices = [
            ec2.BlockDevice(
                device_name="/dev/xvda",
                volume=ec2.BlockDeviceVolume.ebs(
                    volume_gb,
                    volume_type=ec2.EbsDeviceVolumeType.GP3,
                ),
            )
        ]

        def make_instance(
            name: str, instance_type: str, script_path: Path
        ) -> ec2.Instance:
            return ec2.Instance(
                self,
                name,
                instance_type=ec2.InstanceType(instance_type),
                machine_image=ami,
                vpc=vpc,
                vpc_subnets=ec2.SubnetSelection(subnet_type=ec2.SubnetType.PUBLIC),
                security_group=sg,
                key_pair=key,
                role=role,
                placement_group=placement_group,
                block_devices=block_devices,
                require_imdsv2=True,
                user_data=ec2.UserData.custom(script_path.read_text()),
            )

        server = make_instance(
            "Server", server_instance_type, PROVISION_DIR / "server_setup.sh"
        )
        client = make_instance(
            "Client", client_instance_type, PROVISION_DIR / "client_setup.sh"
        )
        Tags.of(server).add("cm-rig", "server")
        Tags.of(client).add("cm-rig", "client")

        CfnOutput(self, "ServerPublicIp", value=server.instance_public_ip)
        CfnOutput(self, "ClientPublicIp", value=client.instance_public_ip)
        CfnOutput(self, "ServerPrivateIp", value=server.instance_private_ip)
        CfnOutput(self, "ClientPrivateIp", value=client.instance_private_ip)
        CfnOutput(self, "SshServer", value=f"ssh ec2-user@{server.instance_public_ip}")
        CfnOutput(self, "SshClient", value=f"ssh ec2-user@{client.instance_public_ip}")
        CfnOutput(
            self,
            "RunHint",
            value=(
                "python3 benchmark/orchestrate.py "
                "--server <ServerPublicIp> --client <ClientPublicIp> "
                "--server-private <ServerPrivateIp> --key ~/.ssh/<key>.pem"
            ),
        )
