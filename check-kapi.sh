#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: Contributors to the Gardener project
#
# SPDX-License-Identifier: Apache-2.0

# Polls the shoot kube-apiserver and reports any downtime periods.
# Useful for verifying zero-downtime during a live control plane migration.
#
# Usage:
#   ./hack/check-kapi-availability.sh <path-to-shoot-kubeconfig>
#   INTERVAL=2 ./hack/check-kapi-availability.sh <path-to-shoot-kubeconfig>
#
# Press Ctrl+C to stop; a summary of downtime periods is printed on exit.

set -euo pipefail

KUBECONFIG="${1:-${KUBECONFIG:-}}"
INTERVAL="${INTERVAL:-1}"

if [[ -z "$KUBECONFIG" ]]; then
  echo "Usage: $0 <shoot-kubeconfig>" >&2
  exit 1
fi

export KUBECONFIG

total=0
failures=0
state="up"
down_since=""
declare -a periods=()

on_exit() {
  local now
  now="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  echo ""
  echo "--- summary ---"
  echo "stopped:        $now"
  echo "total checks:   $total"
  echo "failed checks:  $failures"
  if [[ "$state" == "down" ]]; then
    periods+=("$down_since → $now (still down at exit)")
  fi
  if [[ "${#periods[@]}" -eq 0 ]]; then
    echo "result:         API server was available throughout ✓"
  else
    echo "downtime periods:"
    for p in "${periods[@]}"; do
      echo "  $p"
    done
  fi
}
trap on_exit EXIT

echo "polling shoot kube-apiserver every ${INTERVAL}s  (Ctrl+C to stop)"
echo "started: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
echo "---"

while true; do
  total=$((total + 1))
  ts="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

  if kubectl get --raw /readyz --request-timeout=5s >/dev/null 2>&1; then
    if [[ "$state" == "down" ]]; then
      echo "[$ts] UP  (recovered after $(($(date +%s) - $(date -d "$down_since" +%s 2>/dev/null || date -jf "%Y-%m-%dT%H:%M:%SZ" "$down_since" +%s 2>/dev/null || echo "0")))s)"
      periods+=("$down_since → $ts")
      down_since=""
      state="up"
    fi
  else
    failures=$((failures + 1))
    if [[ "$state" == "up" ]]; then
      down_since="$ts"
      state="down"
      echo "[$ts] DOWN"
    else
      echo "[$ts] DOWN (still)"
    fi
  fi

  sleep "$INTERVAL"
done