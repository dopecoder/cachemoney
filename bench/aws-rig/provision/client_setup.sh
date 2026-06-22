#!/usr/bin/env bash
# client_setup.sh — provision the load generator on Ubuntu 24.04 (noble, arm64).
# Readily-available builds (no source compilation): memtier-benchmark + redis tooling from
# the redis.io apt repo, plus observability tools and Python. Drops a sentinel.
#
# Watch:  ssh ubuntu@<client> tail -f /var/log/cm-setup.log
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a NEEDRESTART_SUSPEND=1

exec > >(tee -a /var/log/cm-setup.log) 2>&1
echo "=== cm client setup start $(date -u) ==="

# Ubuntu's first-boot unattended-upgrades / apt-daily hold the dpkg lock; stop them and make
# every apt-get WAIT for the lock (up to 600s) instead of deadlocking user-data.
echo 'DPkg::Lock::Timeout "600";' >/etc/apt/apt.conf.d/99cmbench-lock
systemctl stop apt-daily.timer apt-daily-upgrade.timer apt-daily.service \
  apt-daily-upgrade.service unattended-upgrades.service 2>/dev/null || true
systemctl disable apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true

apt-get update -y
apt-get install -y ca-certificates curl gnupg lsb-release sysstat numactl python3 python3-pip

# --- redis.io apt repo (memtier-benchmark + redis-cli/redis-benchmark live here) ----
install -m 0755 -d /usr/share/keyrings
curl -fsSL https://packages.redis.io/gpg | gpg --batch --yes --dearmor -o /usr/share/keyrings/redis-archive-keyring.gpg
chmod 0644 /usr/share/keyrings/redis-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/redis-archive-keyring.gpg] https://packages.redis.io/deb $(lsb_release -cs) main" \
  >/etc/apt/sources.list.d/redis.list
apt-get update -y

apt-get install -y memtier-benchmark redis-tools

# Match the server's connection/fd tuning so the generator is never the artificial limit.
cat >/etc/sysctl.d/99-cmbench.conf <<'EOF'
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_tw_reuse = 1
net.core.somaxconn = 65535
fs.file-max = 2000000
EOF
sysctl --system || true
cat >/etc/security/limits.d/99-cmbench.conf <<'EOF'
* soft nofile 1048576
* hard nofile 1048576
EOF

echo "memtier: $(memtier_benchmark --version | head -1)"
echo "redis-cli: $(redis-cli --version)"
echo "=== cm client setup done $(date -u) ==="
touch /opt/cm-setup-done
