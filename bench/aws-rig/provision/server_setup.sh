#!/usr/bin/env bash
# server_setup.sh — provision the server-under-test on Amazon Linux 2023 (arm64).
# Applies benchmark-friendly kernel tuning FIRST, then builds redis 7.4, valkey 8.1, and
# (best-effort) pogocache 1.3.1 from source at pinned versions, installs observability
# tooling, and drops a sentinel.
#
# Runs from EC2 user-data. Watch progress:  ssh ec2-user@<server> tail -f /var/log/cm-setup.log
# Done when /opt/cm-setup-done exists.  cachemoney is uploaded separately (deploy_cachemoney.sh).
set -euxo pipefail

REDIS_VERSION="${REDIS_VERSION:-7.4.2}"
VALKEY_VERSION="${VALKEY_VERSION:-8.1.1}"
POGOCACHE_VERSION="${POGOCACHE_VERSION:-1.3.1}"

exec > >(tee -a /var/log/cm-setup.log) 2>&1
echo "=== cm server setup start $(date -u) redis=$REDIS_VERSION valkey=$VALKEY_VERSION pogo=$POGOCACHE_VERSION ==="

dnf -y groupinstall "Development Tools" || true
dnf -y install gcc gcc-c++ make git tar wget which \
  openssl-devel pcre2-devel libevent-devel zlib-devel \
  autoconf automake libtool pkgconf jemalloc-devel \
  sysstat numactl util-linux

# --- swap (so the from-source builds don't OOM on tiny-RAM smoke instances) -----------
mem_mb="$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)"
if [ ! -e /swapfile ] && [ "$mem_mb" -lt 4096 ]; then
  fallocate -l 4G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=4096
  chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
fi

# --- benchmark-friendly kernel tuning (applied BEFORE builds so a build failure can never
#     leave the rig untuned and silently degrade every server's results) ----------------
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

mkdir -p /opt/src && cd /opt/src

# --- redis 7.4 (essential; provides redis-server, redis-cli, redis-benchmark) -------
if [ ! -x /usr/local/bin/redis-server ]; then
  wget -q "https://download.redis.io/releases/redis-${REDIS_VERSION}.tar.gz"
  tar xzf "redis-${REDIS_VERSION}.tar.gz"
  make -C "redis-${REDIS_VERSION}" -j"$(nproc)" BUILD_TLS=no
  make -C "redis-${REDIS_VERSION}" install PREFIX=/usr/local
fi

# --- valkey 8.1 (essential; redis fork; valkey-server, valkey-cli) ------------------
if [ ! -x /usr/local/bin/valkey-server ]; then
  wget -q "https://github.com/valkey-io/valkey/archive/refs/tags/${VALKEY_VERSION}.tar.gz" -O "valkey-${VALKEY_VERSION}.tar.gz"
  tar xzf "valkey-${VALKEY_VERSION}.tar.gz"
  make -C "valkey-${VALKEY_VERSION}" -j"$(nproc)" BUILD_TLS=no
  make -C "valkey-${VALKEY_VERSION}" install PREFIX=/usr/local
fi

# --- pogocache 1.3.1 (BEST-EFFORT: one optional subject must not abort provisioning) -
# If the tag/prefix or Makefile layout differs, this warns and continues; the orchestrator
# will simply record `no-start` for pogocache. Verify the tag + `pogocache --help` on smoke.
if [ ! -x /usr/local/bin/pogocache ]; then
  (
    wget -q "https://github.com/tidwall/pogocache/archive/refs/tags/${POGOCACHE_VERSION}.tar.gz" -O "pogocache-${POGOCACHE_VERSION}.tar.gz"
    tar xzf "pogocache-${POGOCACHE_VERSION}.tar.gz"
    cd "pogocache-${POGOCACHE_VERSION}" && make -j"$(nproc)"
    POGO_BIN="$(find . -maxdepth 2 -type f -name pogocache -perm -u+x | head -1)"
    install -m 0755 "$POGO_BIN" /usr/local/bin/pogocache
  ) || echo "WARN: pogocache build failed — continuing without it (optional subject)"
fi

echo "redis:    $(/usr/local/bin/redis-server --version)"
echo "valkey:   $(/usr/local/bin/valkey-server --version)"
echo "pogocache present: $(command -v pogocache || echo MISSING)"
echo "=== cm server setup done $(date -u) ==="
touch /opt/cm-setup-done
