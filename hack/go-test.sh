#!/usr/bin/env bash
# Runs go test after checking free disk space, so long automated test loops
# stop before filling the disk. Test binaries already isolate HOME and the
# hub config (internal/testenv); do not wrap this in HOME=$(mktemp -d).
#
# Usage: hack/go-test.sh [go test args...]   (default: ./...)
# Env:   MIN_FREE_GB (default 20)
set -euo pipefail

min_free_gb="${MIN_FREE_GB:-20}"
tmp="${TMPDIR:-/tmp}"
free_kb="$(df -Pk "$tmp" | awk 'NR == 2 { print $4 }')"
free_gb=$((free_kb / 1024 / 1024))
if (( free_gb < min_free_gb )); then
	echo "go-test: only ${free_gb}GB free under ${tmp} (need ${min_free_gb}GB); free space before running tests" >&2
	exit 1
fi

if (( $# == 0 )); then
	set -- ./...
fi
exec go test "$@"
