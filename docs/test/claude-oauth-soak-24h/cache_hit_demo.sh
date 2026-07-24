#!/usr/bin/env bash
set -euo pipefail

umask 077

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
run_stamp=$(date -u +%Y%m%dT%H%M%SZ)
session_name=cache-demo
base_prompt="$script_dir/prompts/session-d-payment-reconciliation.md"
followup_prompt="$script_dir/prompts/cache-demo-followup.md"

: "${SOAK_BASE_URL:?set SOAK_BASE_URL before running the cache demo}"
: "${SOAK_API_KEY:?set SOAK_API_KEY before running the cache demo}"
: "${SOAK_ACCOUNT_ID:?set SOAK_ACCOUNT_ID to the Claude account database ID}"

for command_name in curl jq awk; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "required command not found: $command_name" >&2
    exit 69
  fi
done

if [[ ! -f $base_prompt || ! -f $followup_prompt ]]; then
  echo "cache demo prompt files are missing" >&2
  exit 66
fi
if (( $(wc -c <"$base_prompt") < 16000 )); then
  echo "the cache demo base prompt is unexpectedly short" >&2
  exit 65
fi

export SOAK_OUTPUT_DIR=${CACHE_DEMO_OUTPUT_DIR:-"$script_dir/demo-runs/$run_stamp"}
if [[ -d $SOAK_OUTPUT_DIR ]] && [[ -n $(find "$SOAK_OUTPUT_DIR" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
  echo "CACHE_DEMO_OUTPUT_DIR is not empty; choose a new directory: $SOAK_OUTPUT_DIR" >&2
  exit 73
fi
mkdir -p "$SOAK_OUTPUT_DIR/prompts"

export SOAK_PARALLEL_SESSIONS=0
export SOAK_STREAM=false
export SOAK_SESSION_IDENTITIES_FILE="$SOAK_OUTPUT_DIR/session-identities.json"
unset SOAK_MAX_TOKENS SOAK_DRY_RUN \
  SOAK_CONFIRM_BEFORE_SEND SOAK_CONFIRM_TIMEOUT_SECONDS \
  SOAK_UPSTREAM_APPROVAL_REQUIRED SOAK_UPSTREAM_APPROVAL_DIR \
  SOAK_UPSTREAM_APPROVAL_TOKEN SOAK_UPSTREAM_PREVIEW_PAGER

first_prompt="$SOAK_OUTPUT_DIR/prompts/first-request.md"
cp -- "$base_prompt" "$first_prompt"
printf '\n\nCache demo run identifier: %s\n' "$run_stamp" >>"$first_prompt"

echo "cache demo output: $SOAK_OUTPUT_DIR"
echo "sending request 1 to create a cacheable long prefix"
"$script_dir/send_turn.sh" "$session_name" "$first_prompt" cold

manifest_file="$SOAK_OUTPUT_DIR/manifest.tsv"
first_request_id=$(awk -F '\t' -v session="$session_name" \
  'NR > 1 && $4 == session && $5 == "1" { print $1; exit }' "$manifest_file")
if [[ -z $first_request_id ]]; then
  echo "request 1 is missing from the manifest" >&2
  exit 1
fi
first_response="$SOAK_OUTPUT_DIR/responses/$first_request_id.json"
first_cache_create=$(jq -r '.usage.cache_creation_input_tokens // 0' "$first_response")
first_cache_read=$(jq -r '.usage.cache_read_input_tokens // 0' "$first_response")
printf 'request 1 accepted: cache_create=%s cache_read=%s response=%s\n' \
  "$first_cache_create" "$first_cache_read" "$first_response"
if (( first_cache_create <= 0 && first_cache_read <= 0 )); then
  echo "request 1 neither created nor read a prompt cache; request 2 will not be sent" >&2
  exit 1
fi

echo "building request 2 from the complete request-1 user/assistant history"
set +e
SOAK_CONFIRM_BEFORE_SEND=1 \
SOAK_CONFIRM_TIMEOUT_SECONDS=${CACHE_DEMO_CONFIRM_TIMEOUT_SECONDS:-240} \
  "$script_dir/send_turn.sh" "$session_name" "$followup_prompt" hit
second_rc=$?
set -e
if (( second_rc != 0 )); then
  second_request=$(find "$SOAK_OUTPUT_DIR/requests" -type f -name '*-cache-demo-t2-*.json' -print | sort | tail -n 1)
  if (( second_rc == 75 )); then
    echo "request 2 was prepared but not sent; rerun the whole demo before the 5m cache test: $second_request" >&2
  else
    echo "request 2 failed with exit code $second_rc" >&2
  fi
  exit "$second_rc"
fi

second_request_id=$(awk -F '\t' -v session="$session_name" \
  'NR > 1 && $4 == session && $5 == "2" { print $1; exit }' "$manifest_file")
if [[ -z $second_request_id ]]; then
  echo "request 2 is missing from the manifest" >&2
  exit 1
fi
second_response="$SOAK_OUTPUT_DIR/responses/$second_request_id.json"
second_cache_create=$(jq -r '.usage.cache_creation_input_tokens // 0' "$second_response")
second_cache_read=$(jq -r '.usage.cache_read_input_tokens // 0' "$second_response")
printf 'request 2 accepted: cache_create=%s cache_read=%s response=%s\n' \
  "$second_cache_create" "$second_cache_read" "$second_response"

if (( second_cache_read > 0 )); then
  echo "CACHE HIT CONFIRMED: request 2 reported cache_read_input_tokens=$second_cache_read"
  exit 0
fi

echo "CACHE MISS: request 2 reported cache_read_input_tokens=0" >&2
exit 2
