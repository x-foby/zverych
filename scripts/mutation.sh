#!/usr/bin/env bash
# Mutation testing: go-mutesting generates mutants, scripts/mutation tests them in
# parallel via `go test -overlay` (the working tree is never modified).
#
# Usage: scripts/mutation.sh [flags] [packages]      e.g. scripts/mutation.sh -min-msi 0.85
#        scripts/mutation.sh -h                       for all flags
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
exec go run ./scripts/mutation "$@"
