#!/usr/bin/env bash
# Renders the Helm chart with representative values and asserts on the
# output. Usage: hack/test-chart.sh [chart-dir]
set -euo pipefail

chart="${1:-charts/kubeling}"
helm="${HELM:-helm}"
failures=0

render() {
  "$helm" template kubeling "$chart" --namespace kube-system "$@"
}

expect() {
  local description="$1" pattern="$2" output="$3"
  if grep -qE -- "$pattern" <<<"$output"; then
    echo "ok   - $description"
  else
    echo "FAIL - $description (no match for: $pattern)"
    failures=$((failures + 1))
  fi
}

reject() {
  local description="$1" pattern="$2" output="$3"
  if grep -qE -- "$pattern" <<<"$output"; then
    echo "FAIL - $description (unexpected match for: $pattern)"
    failures=$((failures + 1))
  else
    echo "ok   - $description"
  fi
}

out="$(render)"
expect "default: renders the Deployment" '^kind: Deployment$' "$out"
expect "default: uses the chart image repository" 'image: "ghcr.io/netztronaut/kubeling:latest"' "$out"
reject "default: creates no ConfigMap" '^kind: ConfigMap$' "$out"
reject "default: sets no CONFIGMAP env" 'name: CONFIGMAP' "$out"
reject "default: passes no --provider-id" '--provider-id' "$out"
reject "default: sets no imagePullPolicy" 'imagePullPolicy:' "$out"
reject "default: requires no nodeSelector" 'nodeSelector:' "$out"
expect "default: prefers control-plane nodes" 'preferredDuringSchedulingIgnoredDuringExecution:' "$out"
expect "default: tolerates the control-plane taint" 'key: node-role.kubernetes.io/control-plane' "$out"

out="$(render --values "$chart/ci/rules-values.yaml")"
expect "config: creates the ConfigMap" '^kind: ConfigMap$' "$out"
expect "config: renders every rule map" '^    (initialization|externalIPs|labels|annotations):$' "$out"
expect "config: points the controller at the chart ConfigMap" 'value: "kubeling-config"' "$out"

out="$(render --set configMap=other/rules)"
reject "configMap: creates no ConfigMap" '^kind: ConfigMap$' "$out"
expect "configMap: points the controller at the referenced ConfigMap" 'value: "other/rules"' "$out"

out="$(render --values "$chart/ci/rules-values.yaml" --set configMap=other/rules)"
expect "config and configMap: config wins" 'value: "kubeling-config"' "$out"

out="$(render --set leaderElection.enabled=false --set rbac.create=true)"
reject "leader election disabled: creates no Role" '^kind: Role$' "$out"
expect "leader election disabled: passes the flag" '--leader-elect=false' "$out"

if [ "$failures" -gt 0 ]; then
  echo "$failures chart test(s) failed"
  exit 1
fi
