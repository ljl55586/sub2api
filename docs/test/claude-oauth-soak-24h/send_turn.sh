#!/usr/bin/env bash
set -euo pipefail

umask 077

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <session-name> <prompt-file> <cold|hit|ttl_miss>" >&2
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
max_tokens=${SOAK_MAX_TOKENS-}
stream=${SOAK_STREAM:-false}
user_agent=${SOAK_USER_AGENT:-soak-curl/1.0}
endpoint="${SOAK_BASE_URL%/}/v1/messages"
identity_file=${SOAK_SESSION_IDENTITIES_FILE:-"$output_dir/session-identities.json"}
confirm_before_send=${SOAK_CONFIRM_BEFORE_SEND:-0}
confirm_timeout_seconds=${SOAK_CONFIRM_TIMEOUT_SECONDS:-240}
cache_ttl_seconds=${SOAK_CACHE_TTL_SECONDS:-3600}

if [[ -n $max_tokens && ! $max_tokens =~ ^[1-9][0-9]*$ ]]; then
  echo "SOAK_MAX_TOKENS must be unset, empty, or a positive integer" >&2
  exit 64
fi
case "$stream" in
  true|false) ;;
  *)
    echo "SOAK_STREAM must be true or false" >&2
    exit 64
    ;;
esac
case "$confirm_before_send" in
  0|1) ;;
  *)
    echo "SOAK_CONFIRM_BEFORE_SEND must be 0 or 1" >&2
    exit 64
    ;;
esac
if [[ ! $confirm_timeout_seconds =~ ^[0-9]+$ ]]; then
  echo "SOAK_CONFIRM_TIMEOUT_SECONDS must be a non-negative integer; 0 waits indefinitely" >&2
  exit 64
fi
if [[ ! $cache_ttl_seconds =~ ^[1-9][0-9]*$ ]]; then
  echo "SOAK_CACHE_TTL_SECONDS must be a positive integer" >&2
  exit 64
fi
if [[ $confirm_before_send == 1 && ${SOAK_DRY_RUN:-0} == 1 ]]; then
  echo "SOAK_CONFIRM_BEFORE_SEND=1 cannot be combined with SOAK_DRY_RUN=1" >&2
  exit 64
fi

mkdir -p "$output_dir"/{control,requests,responses,headers,state,tmp}
if [[ ! -f $identity_file ]] || \
  ! jq -e --arg session "$session_name" '.sessions[$session].session_id | type == "string"' "$identity_file" >/dev/null 2>&1; then
  "$script_dir/init_session_identities.sh" "$session_name" >/dev/null
fi

metadata_user_id=$(jq -er --arg session "$session_name" '
  .device_id as $device_id
  | .sessions[$session].session_id as $session_id
  | select($device_id | test("^[a-f0-9]{64}$"))
  | select($session_id | test("^[a-f0-9]{8}-[a-f0-9]{4}-[1-5][a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$"))
  | {device_id: $device_id, account_uuid: "", session_id: $session_id}
  | tojson
' "$identity_file") || {
  echo "valid session identity not found for $session_name in $identity_file" >&2
  exit 65
}

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
wire_response_file="$output_dir/responses/$request_id.raw"
header_file="$output_dir/headers/$request_id.headers"
manifest_file="$output_dir/manifest.tsv"

jq -n \
  --slurpfile history "$tmp_history" \
  --rawfile prompt "$prompt_file" \
  --arg model "$model" \
  --arg metadata_user_id "$metadata_user_id" \
  --arg max_tokens "$max_tokens" \
  --argjson stream "$stream" \
  '({
    model: $model,
    stream: $stream,
    metadata: {user_id: $metadata_user_id},
    messages: ($history[0] + [{
      role: "user",
      content: [{
        type: "text",
        text: $prompt,
        cache_control: {type: "ephemeral", ttl: "5m"}
      }]
    }])
  } + if $max_tokens == "" then {} else {max_tokens: ($max_tokens | tonumber)} end)' >"$request_file"

if [[ ${SOAK_DRY_RUN:-0} == 1 ]]; then
  echo "dry-run request created: $request_file"
  exit 0
fi

if [[ $confirm_before_send == 1 ]]; then
  printf '\n========== PREPARED REQUEST: NOT SENT ==========\n'
  printf 'file: %s\n' "$request_file"
  if (( confirm_timeout_seconds == 0 )); then
    printf 'confirmation timeout: none\n'
  else
    printf 'confirmation timeout: %ss\n' "$confirm_timeout_seconds"
  fi
  jq . "$request_file"
  printf '========== END PREPARED REQUEST ==========\n\n'
  printf 'Type exactly SEND REQUEST to send this file: '
  confirmation=""
  if (( confirm_timeout_seconds == 0 )); then
    if ! IFS= read -r confirmation; then
      printf '\nconfirmation input closed; request was not sent: %s\n' "$request_file" >&2
      exit 75
    fi
  elif ! IFS= read -r -t "$confirm_timeout_seconds" confirmation; then
    printf '\nconfirmation timed out or input closed; request was not sent: %s\n' "$request_file" >&2
    exit 75
  fi
  if [[ $confirmation != "SEND REQUEST" ]]; then
    echo "confirmation did not match; request was not sent: $request_file" >&2
    exit 75
  fi
  if [[ -f "$output_dir/control/STOP" ]]; then
    echo "STOP was requested while awaiting confirmation; request was not sent: $request_file" >&2
    exit 75
  fi
  if [[ ${SOAK_DEADLINE_EPOCH:-} =~ ^[0-9]+$ ]] &&
    (( $(date +%s) >= SOAK_DEADLINE_EPOCH )); then
    echo "the run deadline passed while awaiting confirmation; request was not sent: $request_file" >&2
    exit 75
  fi
  if [[ $expected_cache == hit && -f $manifest_file ]]; then
    last_finished=$(awk -F '\t' -v session="$session_name" \
      'NR > 1 && $4 == session { value=$3 } END { print value }' "$manifest_file")
    if [[ $last_finished =~ ^[0-9]+$ ]] &&
      (( $(date +%s) - last_finished >= cache_ttl_seconds )); then
      expected_cache=ttl_miss
      echo "cache expectation adjusted after confirmation: hit -> ttl_miss"
    fi
  fi
  echo "confirmation accepted; sending the prepared request unchanged"
fi

companion_delay_seconds=0
curl_headers=(
  --header "Authorization: Bearer $SOAK_API_KEY"
  --header "anthropic-version: 2023-06-01"
  --header "Content-Type: application/json"
  --header "User-Agent: $user_agent"
)
if [[ $stream == true && $turn_number == 1 ]]; then
  echo "session startup profile: session=$session_name asynchronous_companions synthetic_delay=0s"
fi

started_epoch=$(date +%s)
set +e
curl_metrics=$(curl \
  --silent \
  --show-error \
  --connect-timeout 15 \
  --max-time 900 \
  --request POST "$endpoint" \
  "${curl_headers[@]}" \
  --data-binary "@$request_file" \
  --dump-header "$header_file" \
  --output "$wire_response_file" \
  --write-out $'%{http_code}\t%{time_total}')
curl_exit=$?
set -e
finished_epoch=$(date +%s)

http_code=000
time_total=0
if [[ -n $curl_metrics ]]; then
  IFS=$'\t' read -r http_code time_total <<<"$curl_metrics"
fi

response_parse_failed=0
if [[ -f $wire_response_file ]]; then
  if [[ $http_code =~ ^2[0-9][0-9]$ ]] &&
    { grep -Eiq '^Content-Type:.*text/event-stream' "$header_file" ||
      grep -Eq '^(event|data):' "$wire_response_file"; }; then
    if ! "$script_dir/parse_anthropic_sse.sh" "$wire_response_file" "$response_file"; then
      response_parse_failed=1
    fi
  else
    cp -- "$wire_response_file" "$response_file"
  fi
else
  : >"$response_file"
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
  printf '%s\n' $'request_id\tstarted_epoch\tfinished_epoch\tsession\tturn\texpected_cache\thttp_code\ttime_total_s\tinput_tokens\toutput_tokens\tcache_creation_input_tokens\tcache_read_input_tokens\tupstream_request_id\tstream\tcompanion_delay_seconds' >"$manifest_file"
fi
printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
  "$request_id" "$started_epoch" "$finished_epoch" "$session_name" "$turn_number" \
  "$expected_cache" "$http_code" "$time_total" "$input_tokens" "$output_tokens" \
  "$cache_creation" "$cache_read" "$upstream_request_id" "$stream" \
  "$companion_delay_seconds" >>"$manifest_file"
if [[ ${SOAK_PARALLEL_SESSIONS:-0} == 1 ]]; then
  flock -u 8
  exec 8>&-
fi

if (( curl_exit != 0 )); then
  echo "curl failed with exit code $curl_exit; state was not advanced" >&2
  exit 1
fi
if (( response_parse_failed != 0 )); then
  echo "streaming response could not be reconstructed; state was not advanced (raw=$wire_response_file)" >&2
  exit 1
fi

if [[ ! $http_code =~ ^2[0-9][0-9]$ ]]; then
  error_message=$(jq -r '.error.message // .message // "non-JSON or unknown error"' "$response_file" 2>/dev/null || true)
  echo "HTTP $http_code: $error_message; state was not advanced" >&2
  exit 1
fi

if ! jq -e '.content | type == "array"' "$response_file" >/dev/null 2>&1; then
  echo "successful response has no content array; state was not advanced" >&2
  exit 1
fi

stop_reason=$(jq -r '.stop_reason // ""' "$response_file")
content_types=$(jq -r '[.content[] | .type // "<missing>"] | join(",")' "$response_file")
if [[ $stop_reason == max_tokens ]]; then
  echo "response was truncated at max_tokens (content_types=${content_types:-none}); state was not advanced" >&2
  exit 1
fi
if [[ $stop_reason != end_turn ]]; then
  echo "unexpected stop_reason=${stop_reason:-missing} (content_types=${content_types:-none}); state was not advanced" >&2
  exit 1
fi
if ! jq -e '
  any(.content[];
    .type == "text"
    and (.text | type == "string")
    and ((.text | gsub("\\s"; "") | length) > 0)
  )
' "$response_file" >/dev/null 2>&1; then
  echo "end_turn response has no non-empty text block (content_types=${content_types:-none}); state was not advanced" >&2
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
