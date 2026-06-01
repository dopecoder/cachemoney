#!/usr/bin/env bash
# Thin convenience wrapper around `make bench-compare` (run from anywhere in the repo).
set -euo pipefail
cd "$(dirname "$0")/.."
exec make bench-compare
