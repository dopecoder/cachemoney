#!/usr/bin/env bash
# server_setup.sh — provision the server-under-test on Ubuntu 24.04 (noble, arm64).
# Uses readily-available builds (no source compilation):
#   redis 7.4.x   -> apt (packages.redis.io)
#   valkey 8.1.1  -> official prebuilt binary (download.valkey.io)
#   pogocache 1.3.1 -> official prebuilt binary (github releases)
# Applies kernel tuning and drops a sentinel. cachemoney is uploaded separately.
#
# Runs from EC2 user-data. Watch:  ssh ubuntu@<server> tail -f /var/log/cm-setup.log
# Done when /opt/cm-setup-done exists.
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a NEEDRESTART_SUSPEND=1

REDIS_TRAIN="${REDIS_TRAIN:-7.4}"
VALKEY_VERSION="${VALKEY_VERSION:-8.1.1}"
POGOCACHE_VERSION="${POGOCACHE_VERSION:-1.3.1}"

exec > >(tee -a /var/log/cm-setup.log) 2>&1
echo "=== cm server setup start $(date -u) redis~=$REDIS_TRAIN valkey=$VALKEY_VERSION pogo=$POGOCACHE_VERSION ==="

# Ubuntu's first-boot unattended-upgrades / apt-daily hold the dpkg lock; stop them and make
# every apt-get WAIT for the lock (up to 600s) instead of deadlocking user-data.
echo 'DPkg::Lock::Timeout "600";' >/etc/apt/apt.conf.d/99cmbench-lock
systemctl stop apt-daily.timer apt-daily-upgrade.timer apt-daily.service \
  apt-daily-upgrade.service unattended-upgrades.service 2>/dev/null || true
systemctl disable apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true

apt-get update -y
apt-get install -y ca-certificates curl gnupg lsb-release tar sysstat numactl util-linux

# --- redis.io apt repo (redis + memtier live here) ---------------------------------
install -m 0755 -d /usr/share/keyrings
curl -fsSL https://packages.redis.io/gpg | gpg --batch --yes --dearmor -o /usr/share/keyrings/redis-archive-keyring.gpg
chmod 0644 /usr/share/keyrings/redis-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/redis-archive-keyring.gpg] https://packages.redis.io/deb $(lsb_release -cs) main" \
  >/etc/apt/sources.list.d/redis.list
apt-get update -y

# --- redis 7.4.x (apt; pin to the train, hold, and disable the auto-started service) --
REDIS_VER="$(apt-cache madison redis-server | awk -v t="$REDIS_TRAIN" '!seen && $3 ~ ("[0-9]+:" t "\\.") {print $3; seen=1}')"
[ -n "$REDIS_VER" ] || {
  echo "ERROR: no redis ${REDIS_TRAIN}.x in the apt repo"
  exit 1
}
echo "installing redis-server=$REDIS_VER"
apt-get install -y --allow-downgrades "redis-server=$REDIS_VER" "redis-tools=$REDIS_VER"
apt-mark hold redis-server redis-tools
systemctl disable --now redis-server || true
ln -sf /usr/bin/redis-server /usr/local/bin/redis-server

# --- valkey 8.1.1 (official prebuilt binary, noble arm64) ---------------------------
curl -fsSL "https://download.valkey.io/releases/valkey-${VALKEY_VERSION}-noble-arm64.tar.gz" -o /tmp/valkey.tgz
mkdir -p /opt/valkey && tar xzf /tmp/valkey.tgz -C /opt/valkey --strip-components=1
ln -sf "$(find /opt/valkey -type f -name valkey-server -print -quit)" /usr/local/bin/valkey-server
ln -sf "$(find /opt/valkey -type f -name valkey-cli -print -quit)" /usr/local/bin/valkey-cli

# --- pogocache 1.3.1 (official prebuilt binary, arm64 glibc) ------------------------
curl -fsSL "https://github.com/tidwall/pogocache/releases/download/${POGOCACHE_VERSION}/pogocache-linux-arm64.tar.gz" -o /tmp/pogo.tgz
mkdir -p /opt/pogocache && tar xzf /tmp/pogo.tgz -C /opt/pogocache
install -m 0755 "$(find /opt/pogocache -type f -name pogocache -print -quit)" /usr/local/bin/pogocache

# --- benchmark-friendly kernel tuning ----------------------------------------------
cat >/etc/sysctl.d/99-cmbench.conf <<'EOF'
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
net.core.netdev_max_backlog = 250000
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_tw_reuse = 1
fs.file-max = 2000000
EOF
sysctl --system || true
cat >/etc/security/limits.d/99-cmbench.conf <<'EOF'
* soft nofile 1048576
* hard nofile 1048576
EOF

echo "redis:     $(/usr/local/bin/redis-server --version | head -c60)"
echo "valkey:    $(/usr/local/bin/valkey-server --version | head -c60)"
echo "pogocache: $(/usr/local/bin/pogocache --version 2>/dev/null || echo present)"
echo "=== cm server setup done $(date -u) ==="
touch /opt/cm-setup-done
