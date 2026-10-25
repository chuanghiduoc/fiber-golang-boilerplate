#!/usr/bin/env bash
#
# Compatibility wrapper: scaffold a module with the Go generator (cmd/gen).
# Kept so existing docs and habits keep working; prefer `make gen module=<name>`.
#
# Usage:
#   scripts/new-module.sh <name>
#   scripts/new-module.sh --dry-run <name>
#
# See `go run ./cmd/gen -h` for resources, commands, and queries.

set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"
exec go run ./cmd/gen module "$@"
