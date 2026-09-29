#!/usr/bin/env bash
# Sends requests to the loadtarget Service (hack/testdata/loadtarget.yaml) from inside the
# cluster, so they spread over its pods the way real traffic does.
#
#   hack/load.sh [seconds] [requests-per-second] [n]
#
# A request costs about n/1e6 seconds of one core (n=100000 is ~0.1), so the pods burn about
#   requests-per-second * n / 1e6   cores
# in total. Defaults: 120 s, 5 req/s, n=100000 -> ~0.5 CPU. Each pod has a 1 CPU limit,
# so more than ~2 cores in total is throttled.
#
#   hack/load.sh 300 10          # ~1 CPU for five minutes
#   hack/load.sh 60 20 200000    # ~4 CPU worth of requests: the pods hit their limit
#
# To throw single requests by hand instead:
#   kubectl port-forward svc/loadtarget 8081:80
#   curl 'localhost:8081/cgi-bin/burn?n=500000'
set -euo pipefail

seconds=${1:-120}
rate=${2:-5}
n=${3:-100000}
name="loadgen-$(date +%s)"

echo "loadtarget: ${rate} req/s x n=${n} for ${seconds}s  (~$(awk "BEGIN{printf \"%.2f\", ${rate}*${n}/1000000}") CPU in total)"

cleanup() { kubectl delete pod "${name}" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

# One request per tick; a slow one does not delay the next, so the rate holds.
kubectl run "${name}" --rm -i --restart=Never --image=busybox:1.36 --quiet -- sh -c "
  interval=\$(awk 'BEGIN{print 1/${rate}}')
  end=\$((\$(date +%s) + ${seconds}))
  sent=0
  while [ \$(date +%s) -lt \$end ]; do
    wget -qO- 'http://loadtarget/cgi-bin/burn?n=${n}' >/dev/null 2>&1 &
    sent=\$((sent + 1))
    sleep \$interval
  done
  wait
  echo \"sent \$sent requests\"
"
