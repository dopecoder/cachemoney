# cachemoney AWS benchmark rig

A two-machine AWS rig to settle, on **isolated hardware**, the questions a co-located rig
can't: does the gnet event loop deliver a redis-class tail, does cachemoney scale vertically,
and how do the five servers really compare. Design and rationale: **[BENCHMARK-PLAN.md](BENCHMARK-PLAN.md)**.

- **Server-under-test:** `r8g.metal-24xl` (Graviton4, 96 cores, **bare metal**).
- **Load generator:** `c8g.24xlarge` (Graviton4, 96 cores).
- Same VPC + cluster placement group; load flows over the **private IP**; orchestration is
  SSH control-plane from your laptop. Start on small instances, then scale up.

```
bench/aws-rig/
├── BENCHMARK-PLAN.md      # the experiment design (what/how) — read this first
├── app.py  cdk.json  requirements.txt  rig/rig_stack.py   # CDK (VPC, SG, placement group, 2 instances)
├── provision/             # native installs baked into user-data (no Docker)
│   ├── server_setup.sh    #   redis 7.4 + valkey 8.1 + pogocache 1.3.1 from source + tuning
│   └── client_setup.sh    #   memtier 2.4.2 + redis tooling + sysstat
├── deploy_cachemoney.sh   # build cachemoney arm64 (with the gnet spike) + upload
└── benchmark/
    ├── orchestrate.py     # the sweep: start DBs, drive load, capture validity stats -> results.csv
    └── analyze.py         # results.csv -> per-experiment comparison tables
```

## Prerequisites

- **AWS account + credentials.** `aws configure` (or `AWS_PROFILE`); confirm with
  `aws sts get-caller-identity`. Set your region: `export CDK_DEFAULT_REGION=us-east-1`
  and `export CDK_DEFAULT_ACCOUNT=$(aws sts get-caller-identity --query Account --output text)`.
- **CDK CLI** (Node): `npm install -g aws-cdk` (the app itself is Python).
- **Python 3.9+** and **Go** (to cross-build cachemoney for arm64).
- **An EC2 key pair** you hold the private key for:

  ```bash
  aws ec2 create-key-pair --key-name cm-rig \
    --query KeyMaterial --output text > ~/.ssh/cm-rig.pem && chmod 600 ~/.ssh/cm-rig.pem
  ```

## One-time setup

```bash
cd bench/aws-rig
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
cdk bootstrap          # once per account+region
```

## 1. Smoke on tiny instances (cents — do this first)

Defaults are **`t4g.medium` (server) + `t4g.small` (client)** — the cheapest ARM burstable
instances (~1-4¢/hr each), so the smoke exercises the *same arm64 build and ARM provisioning*
as the real run. (AWS free tier is x86 `t3.micro`; there is no free Graviton instance, and
an x86 smoke wouldn't validate the arm64 path — so we use tiny ARM instead.)

```bash
# Tiny ARM defaults (t4g.medium + t4g.small). Lock SSH to your IP.
cdk deploy -c key_name=cm-rig -c allowed_ssh_cidr=$(curl -s ifconfig.me)/32
```

CDK prints `ServerPublicIp`, `ClientPublicIp`, `ServerPrivateIp`. Wait for provisioning
(source builds take a few minutes; the scripts add swap so they don't OOM on 2 GB) until the
sentinel exists on **both** hosts:

```bash
ssh -i ~/.ssh/cm-rig.pem ec2-user@<ServerPublicIp> 'tail -f /var/log/cm-setup.log'   # Ctrl-C at "done"
ssh -i ~/.ssh/cm-rig.pem ec2-user@<ServerPublicIp> 'test -f /opt/cm-setup-done && echo READY'
ssh -i ~/.ssh/cm-rig.pem ec2-user@<ClientPublicIp> 'test -f /opt/cm-setup-done && echo READY'
```

Upload cachemoney, then run the reduced matrix:

```bash
./deploy_cachemoney.sh <ServerPublicIp> ~/.ssh/cm-rig.pem
python3 benchmark/orchestrate.py \
  --server <ServerPublicIp> --client <ClientPublicIp> --server-private <ServerPrivateIp> \
  --key ~/.ssh/cm-rig.pem --quick
python3 benchmark/analyze.py results.csv
```

`--quick` shrinks the matrix and timings so you can validate the whole pipeline end-to-end
for cents before paying for metal. **Expect most smoke rows to be tagged `rig-limited` or
`noisy`** — a 2-vCPU client saturates instantly. That's fine: the smoke proves the
mechanics (deploy, provisioning, cachemoney upload, SSH lifecycle, stats capture, memtier
parsing, CSV, `analyze.py`), not the numbers.

## 2. The real run (metal pair)

```bash
cdk deploy -c key_name=cm-rig -c allowed_ssh_cidr=$(curl -s ifconfig.me)/32 \
  -c server_instance_type=r8g.metal-24xl -c client_instance_type=c8g.24xlarge -c volume_gb=80
```

Bare-metal instances take longer to launch (~10-20 min) and longer to provision. Once both
sentinels are READY:

```bash
./deploy_cachemoney.sh <ServerPublicIp> ~/.ssh/cm-rig.pem
# Pin memtier to most of the client's cores, leaving headroom for its softirq.
python3 benchmark/orchestrate.py \
  --server <ServerPublicIp> --client <ClientPublicIp> --server-private <ServerPrivateIp> \
  --key ~/.ssh/cm-rig.pem --client-cores 0-79 --experiments e1,e2,e3
python3 benchmark/analyze.py results.csv | tee SUMMARY.md
```

## 3. Tear down (stop paying)

```bash
cdk destroy
```

## Cost

On-demand us-east-1, **both** instances, for a ~1-2 h session then destroy: the tiny ARM
smoke (`t4g.medium` + `t4g.small`) is **~5-8¢/hr combined** (a full smoke is well under $1);
the metal pair (`r8g.metal-24xl` + `c8g.24xlarge`) is roughly **$15-25/hr combined**, so a
focused run is ~$20-40. `cdk destroy` removes everything. Spot can cut the metal cost ~60-70%.

## How we keep the numbers honest

Every run captures **server CPU (avg + per-core max), softirq NET_RX/TX delta, NIC
RX/TX + %util, and client CPU** alongside latency/throughput. A row is tagged `valid` only if
the **client isn't saturated** (max core < 85%) and the **NIC is below ~80%** of line rate;
otherwise it's `rig-limited:*` and `analyze.py` keeps it out of the headline tables. Warmup +
3 repeats; runs with CoV > 5% are flagged `noisy`. This is the discipline from
`~/.agents/skills/benchmarking-discipline/` — it's what invalidated every earlier high-core
number, so it's enforced in code here.

## Troubleshooting

- **Provisioning stuck / a server won't start:** `ssh ec2-user@<host> 'cat /var/log/cm-setup.log'`
  and `cat /tmp/cm-srv-<port>.log`. The likeliest fix is a flag-name mismatch for
  `pogocache`/`valkey` — adjust `SERVER_CMDS` in `orchestrate.py` after checking `--help` on
  the box (the templates are centralized at the top of the file for exactly this reason).
- **`no-start` rows:** the readiness probe never got PONG; check the server log and that the
  SG self-rule is in place (it is, by default).
- **Everything `rig-limited:client-cpu`:** the load generator is the bottleneck — give memtier
  more `--client-cores`, raise concurrency, or use a bigger client instance.
- **SSH refused:** confirm `allowed_ssh_cidr` covers your current IP (it changes).
