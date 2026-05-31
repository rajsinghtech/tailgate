#!/usr/bin/env bash
# Full e2e for one cluster IP family: create a kind cluster of that family, build+load
# images, deploy tailgate, run the datapath test (which owns the ephemeral-tailnet
# lifecycle), then optionally tear the cluster down.
#
# Usage: hack/e2e.sh <v4|dual|v6> [--keep]
#   --keep   leave the cluster running afterwards (default: delete it).
#
# The SAME datapath test runs for every family — it curls the peer over BOTH v4 and v6
# through the always-dual-stack veth, so it proves family-independence regardless of the
# cluster's own primary family.
set -euo pipefail
cd "$(dirname "$0")/.."

FAMILY="${1:?usage: hack/e2e.sh <v4|dual|v6> [--keep]}"
KEEP=""
[ "${2:-}" = "--keep" ] && KEEP=1
CLUSTER="tailgate-${FAMILY}"
TAG="${FAMILY}" # fresh cluster per family, so no image-cache staleness to dodge
CONFIG="test/kind/${FAMILY}.yaml"
[ -f "$CONFIG" ] || { echo "no kind config $CONFIG"; exit 1; }

cleanup() { [ -z "$KEEP" ] && kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo ">> [$FAMILY] creating kind cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --config "$CONFIG" --wait 120s
kubectl config use-context "kind-${CLUSTER}" >/dev/null

echo ">> [$FAMILY] building + loading images (tag $TAG)"
ARCH="${ARCH:-arm64}" bash hack/build.sh "$TAG" "$CLUSTER"

echo ">> [$FAMILY] installing CRD + manifests"
# --validate=false: on a v6-only kind cluster the client-side openapi fetch can fail over
# the [::1] loopback on Docker Desktop; the apply itself is fine.
kubectl apply --validate=false -f config/crd/tailscale.rajsingh.info_egressgroups.yaml
# pin the just-built tag into a temp copy of the manifest
tmp="$(mktemp)"
sed -E "s/tailgate-(operator|agent|gateway):[A-Za-z0-9._-]+/tailgate-\1:${TAG}/g" deploy/manifests/tailgate.yaml > "$tmp"
kubectl apply -f "$tmp"
rm -f "$tmp"

# placeholder creds so the operator pod can boot; the test overwrites + restarts it.
kubectl -n tailgate-system create secret generic tailgate-tailnet-creds \
  --from-literal=TS_TAILNET=placeholder \
  --from-literal=TS_OAUTH_CLIENT_ID=placeholder \
  --from-literal=TS_OAUTH_CLIENT_SECRET=placeholder \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n tailgate-system rollout status deploy/tailgate-operator --timeout=120s
kubectl -n tailgate-system rollout status ds/tailgate-agent --timeout=120s

echo ">> [$FAMILY] node IP families:"
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.spec.podCIDRs}{"\n"}{end}'

echo ">> [$FAMILY] running datapath test"
GOWORK=off go test -tags e2e -count=1 -timeout 12m -run TestEgressDatapath ./test/e2e/ -v
echo ">> [$FAMILY] PASS"
