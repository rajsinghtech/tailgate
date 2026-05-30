#!/usr/bin/env bash
# Exchange a GitHub Actions OIDC token for a short-lived Tailscale API access token
# via Tailscale Workload Identity Federation (WIF) — no static OAuth secret.
#
# This is a portable adaptation of tailvoy's integration_test/scripts/exchange-oidc-token.sh.
# It does the whole flow in one shot: mint the GitHub OIDC JWT (when run inside a
# GitHub Actions job), then POST it to the Tailscale token-exchange endpoint and
# print the resulting access token on stdout. Everything else goes to stderr so the
# token can be captured with `TOKEN=$(hack/wif-exchange.sh <client_id>)`.
#
# Usage:
#   hack/wif-exchange.sh <client_id> [jwt]
#
#   <client_id>  The federated identity (OAuth) client ID created under
#                "Settings > Trust credentials" in the Tailscale admin console.
#                The token-exchange audience is always "api.tailscale.com/<client_id>".
#   [jwt]        Optional pre-fetched GitHub OIDC JWT. If omitted, the script mints
#                one using the GitHub Actions OIDC request env vars
#                (ACTIONS_ID_TOKEN_REQUEST_TOKEN / ACTIONS_ID_TOKEN_REQUEST_URL),
#                which are only present when `permissions: id-token: write` is set.
#
# Output: the access token on stdout (single line). Use it immediately as a bearer
# token — it is short-lived and must not be persisted.
#
# Requires: curl, jq.
set -euo pipefail

API_BASE="${TS_API_BASE:-https://api.tailscale.com}"

err() { echo "$@" >&2; }

CLIENT_ID="${1:-}"
JWT="${2:-}"

if [ -z "$CLIENT_ID" ]; then
    err "Error: federated identity client ID is required as the first argument"
    err "Usage: hack/wif-exchange.sh <client_id> [jwt]"
    exit 1
fi

# The token-exchange audience is derived from the client ID. This is what the
# federated credential validates the GitHub JWT's `aud` claim against.
AUDIENCE="api.tailscale.com/${CLIENT_ID}"

# 1. Mint a GitHub Actions OIDC JWT if one wasn't supplied.
#
# GitHub exposes ACTIONS_ID_TOKEN_REQUEST_TOKEN + ACTIONS_ID_TOKEN_REQUEST_URL to a
# job only when it requests `permissions: id-token: write`. We ask GitHub's OIDC
# provider for a token whose `aud` claim equals our Tailscale audience, so the
# federated credential's audience check passes.
if [ -z "$JWT" ]; then
    if [ -z "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ] || [ -z "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]; then
        err "Error: no JWT supplied and not running in a GitHub Actions job with id-token: write."
        err "Either pass a JWT as the second argument or set permissions: id-token: write."
        exit 1
    fi
    err "Requesting GitHub OIDC token (audience=${AUDIENCE})"
    JWT=$(curl -sS \
        -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
        "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=${AUDIENCE}" \
        | jq -r '.value')
    if [ -z "$JWT" ] || [ "$JWT" = "null" ]; then
        err "Error: failed to mint GitHub OIDC token"
        exit 1
    fi
fi

# 2. Exchange the JWT for a Tailscale API access token.
#
# POST /api/v2/oauth/token-exchange with the client_id and the signed JWT, form-encoded.
# Tailscale fetches the issuer's keys, verifies the signature + standard claims, matches
# the JWT's issuer/subject/audience against the federated credential, and (on success)
# returns a short-lived API token carrying the scopes configured for that credential.
err "Exchanging OIDC token at ${API_BASE}/api/v2/oauth/token-exchange"
RESPONSE=$(curl -s -w "\nHTTP_STATUS:%{http_code}" -X POST "${API_BASE}/api/v2/oauth/token-exchange" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "client_id=${CLIENT_ID}" \
    -d "jwt=${JWT}")

HTTP_STATUS=$(echo "$RESPONSE" | tail -n 1 | cut -d: -f2)
RESPONSE_BODY=$(echo "$RESPONSE" | sed '$d')

if [ "$HTTP_STATUS" != "200" ]; then
    err "Error: token exchange failed with status ${HTTP_STATUS}"
    err "Response: ${RESPONSE_BODY}"
    err "Check that the federated credential's issuer/subject/audience match this job."
    exit 1
fi

ACCESS_TOKEN=$(echo "$RESPONSE_BODY" | jq -r '.access_token')

if [ "$ACCESS_TOKEN" = "null" ] || [ -z "$ACCESS_TOKEN" ]; then
    err "Error: no access_token in response"
    err "Response: ${RESPONSE_BODY}"
    exit 1
fi

echo "$ACCESS_TOKEN"
