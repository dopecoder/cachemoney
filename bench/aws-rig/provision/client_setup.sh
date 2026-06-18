#!/usr/bin/env bash
# client_setup.sh — provision the load generator on Amazon Linux 2023 (arm64).
# Builds memtier_benchmark at a pinned version, installs redis tooling (redis-cli for
# readiness probes, redis-benchmark for the pipelined throughput ceiling), observability
# tools, and Python for analysis. Drops a sentinel at /opt/cm-setup-done.
#
# Watch progress:  ssh ec2-user@<client> tail -f /var/log/cm-setup.log
set -euxo pipefail

MEMTIER_VERSION="${MEMTIER_VERSION:-2.4.2}"

exec > >(tee -a /var/log/cm-setup.log) 2>&1
echo "=== cm client setup start $(date -u) memtier=$MEMTIER_VERSION ==="

dnf -y groupinstall "Development Tools" || true
dnf -y install gcc gcc-c++ make git tar wget which \
  autoconf automake libtool pkgconf \
  openssl-devel pcre2-devel libevent-devel zlib-devel \
  sysstat numactl util-linux python3 python3-pip redis6

# --- swap (so the memtier build doesn't OOM on tiny-RAM smoke instances) --------------
mem_mb="$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)"
if [ ! -e /swapfile ] && [ "$mem_mb" -lt 4096 ]; then
  fallocate -l 4G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=4096
  chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
fi

# --- memtier_benchmark (the primary load generator) --------------------------------
if [ ! -x /usr/local/bin/memtier_benchmark ]; then
  mkdir -p /opt/src && cd /opt/src
  git clone --depth 1 --branch "${MEMTIER_VERSION}" https://github.com/RedisLabs/memtier_benchmark.git
  cd memtier_benchmark
  autoreconf -ivf
  ./configure
  make -j"$(nproc)"
  make install
fi

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

echo "memtier: $(/usr/local/bin/memtier_benchmark --version | head -1)"
echo "redis-benchmark: $(redis6-benchmark --version 2>/dev/null || redis-benchmark --version 2>/dev/null || echo MISSING)"
echo "=== cm client setup done $(date -u) ==="
touch /opt/cm-setup-done
