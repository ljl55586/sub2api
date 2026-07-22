#!/usr/bin/env bash
set -euo pipefail

umask 077

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <session-a|session-b|session-c> <prompt-file> <cold|hit|ttl_miss>" >&2
  exit 64
fi

session_name=$1
prompt_file=$2
expected_cache=$3

if [[ ! $session_name =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "invalid session name: $session_name" >&2
  exit 64
fi
case "$expected_cache" in
  cold|hit|ttl_miss) ;;
  *)
    echo "invalid cache expectation: $expected_cache" >&2
    exit 64
    ;;
esac

if [[ ! -f $prompt_file ]]; then
  echo "prompt file not found: $prompt_file" >&2
  exit 66
fi

for command_name in curl jq; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "required command not found: $command_name" >&2
    exit 69
  fi
done

: "${SOAK_BASE_URL:?set SOAK_BASE_URL, for example https://sub2api.example.com}"
: "${SOAK_API_KEY:?set SOAK_API_KEY in the environment; never write it into a file}"

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
output_dir=${SOAK_OUTPUT_DIR:-"$script_dir/output"}
model=${SOAK_MODEL:-claude-opus-4-8}
max_tokens=${SOAK_MAX_TOKENS:-1536}
user_agent=${SOAK_USER_AGENT:-soak-curl/1.0}
endpoint="${SOAK_BASE_URL%/}/v1/messages"

if [[ ! $max_tokens =~ ^[1-9][0-9]*$ ]]; then
  echo "SOAK_MAX_TOKENS must be a positive integer" >&2
  exit 64
fi

mkdir -p "$output_dir"/{control,requests,responses,headers,state,tmp}
state_file="$output_dir/state/$session_name.messages.json"
tmp_history=$(mktemp "$output_dir/tmp/history.XXXXXX")
trap 'rm -f "$tmp_history"' EXIT

if [[ -f $state_file ]]; then
  jq -e 'type == "array"' "$state_file" >/dev/null
  cp "$state_file" "$tmp_history"
else
  printf '[]\n' >"$tmp_history"
fi

turn_number=$(jq '[.[] | select(.role == "user")] | length + 1' "$tmp_history")
timestamp=$(date -u +%Y%m%dT%H%M%SZ)
request_id="${timestamp}-${session_name}-t${turn_number}-$$"
request_file="$output_dir/requests/$request_id.json"
response_file="$output_dir/responses/$request_id.json"
header_file="$output_dir/headers/$request_id.headers"
manifest_file="$output_dir/manifest.tsv"

jq -n \
  --slurpfile history "$tmp_history" \
  --rawfile prompt "$prompt_file" \
  --arg model "$model" \
  --argjson max_tokens "$max_tokens" \
  '{
    model: $model,
    max_tokens: $max_tokens,
    messages: ($history[0] + [{
      role: "user",
      content: [{
        type: "text",
        text: $prompt,
        cache_control: {type: "ephemeral", ttl: "5m"}
      }]
    }])
  }' >"$request_file"

if [[ ${SOAK_DRY_RUN:-0} == 1 ]]; then
  echo "dry-run request created: $request_file"
  exit 0
fi

started_epoch=$(date +%s)
set +e
curl_metrics=$(curl \
  --silent \
  --show-error \
  --connect-timeout 15 \
  --max-time 900 \
  --request POST "$endpoint" \
  --header "Authorization: Bearer $SOAK_API_KEY" \
  --header "anthropic-version: 2023-06-01" \
  --header "Content-Type: application/json" \
  --header "User-Agent: $user_agent" \
  --data-binary "@$request_file" \
  --dump-header "$header_file" \
  --output "$response_file" \
  --write-out $'%{http_code}\t%{time_total}')
curl_exit=$?
set -e
finished_epoch=$(date +%s)

http_code=000
time_total=0
if [[ -n $curl_metrics ]]; then
  IFS=$'\t' read -r http_code time_total <<<"$curl_metrics"
fi

input_tokens=0
output_tokens=0
cache_creation=0
cache_read=0
upstream_request_id=""
if jq -e . "$response_file" >/dev/null 2>&1; then
  input_tokens=$(jq -r '.usage.input_tokens // 0' "$response_file")
  output_tokens=$(jq -r '.usage.output_tokens // 0' "$response_file")
  cache_creation=$(jq -r '.usage.cache_creation_input_tokens // 0' "$response_file")
  cache_read=$(jq -r '.usage.cache_read_input_tokens // 0' "$response_file")
  upstream_request_id=$(jq -r '.id // .request_id // ""' "$response_file")
fi

if [[ ${SOAK_PARALLEL_SESSIONS:-0} == 1 ]]; then
  if ! command -v flock >/dev/null 2>&1; then
    echo "flock is required when SOAK_PARALLEL_SESSIONS=1" >&2
    exit 69
  fi
  exec 8>>"$output_dir/control/manifest.lock"
  flock -x 8
fi
if [[ ! -f $manifest_file ]]; then
  printf '%s\n' $'request_id\tstarted_epoch\tfinished_epoch\tsession\tturn\texpected_cache\thttp_code\ttime_total_s\tinput_tokens\toutput_tokens\tcache_creation_input_tokens\tcache_read_input_tokens\tupstream_request_id' >"$manifest_file"
fi
printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
  "$request_id" "$started_epoch" "$finished_epoch" "$session_name" "$turn_number" \
  "$expected_cache" "$http_code" "$time_total" "$input_tokens" "$output_tokens" \
  "$cache_creation" "$cache_read" "$upstream_request_id" >>"$manifest_file"
if [[ ${SOAK_PARALLEL_SESSIONS:-0} == 1 ]]; then
  flock -u 8
  exec 8>&-
fi

if (( curl_exit != 0 )); then
  echo "curl failed with exit code $curl_exit; state was not advanced" >&2
  exit 1
fi

if [[ ! $http_code =~ ^2[0-9][0-9]$ ]]; then
  error_message=$(jq -r '.error.message // .message // "non-JSON or unknown error"' "$response_file" 2>/dev/null || true)
  echo "HTTP $http_code: $error_message; state was not advanced" >&2
  exit 1
fi

if ! jq -e '.content | type == "array"' "$response_file" >/dev/null; then
  echo "successful response has no content array; state was not advanced" >&2
  exit 1
fi

next_state=$(mktemp "$output_dir/tmp/state.XXXXXX")
trap 'rm -f "$tmp_history" "$next_state"' EXIT
jq -n \
  --slurpfile history "$tmp_history" \
  --slurpfile response "$response_file" \
  --rawfile prompt "$prompt_file" \
  '$history[0]
   + [{role: "user", content: [{type: "text", text: $prompt}]}]
   + [{role: "assistant", content: $response[0].content}]' >"$next_state"
mv "$next_state" "$state_file"

echo "ok session=$session_name turn=$turn_number expected=$expected_cache http=$http_code cache_create=$cache_creation cache_read=$cache_read response=$response_file"
