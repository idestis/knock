#!/usr/bin/env bash
# Lints and renders charts/knock-relay with its default values and every file in ci/,
# checks that settings knockd can't run with fail the render, and that the chart's
# version and appVersion equal MARKETING_VERSION. Used by `task chart:lint` and CI.
set -euo pipefail
cd "$(dirname "$0")/.."

chart=charts/knock-relay
want=$(sed -n 's/^MARKETING_VERSION = //p' mac/Config/Version.xcconfig)
v=$(sed -n 's/^version: //p' $chart/Chart.yaml)
a=$(sed -n 's/^appVersion: //p' $chart/Chart.yaml | tr -d '"')
if [ "$v" != "$want" ] || [ "$a" != "$want" ]; then
  echo "Chart.yaml version $v and appVersion $a must equal MARKETING_VERSION $want (task app:bump sets all three)"
  exit 1
fi

helm lint --strict $chart
helm template knock-relay $chart >/dev/null
for f in $chart/ci/*.yaml; do
  echo "values: $f"
  helm lint --strict $chart -f "$f" >/dev/null
  helm template knock-relay $chart -f "$f" >/dev/null
done

# Settings knockd can't run with must fail the render.
for bad in replicaCount=2 store.type=valkey allowAnonymous=false auth.github.clientId=x drainGrace=90s; do
  if helm template knock-relay $chart --set "$bad" >/dev/null 2>&1; then
    echo "expected --set $bad to fail the render"
    exit 1
  fi
done
echo "chart $v ok"
