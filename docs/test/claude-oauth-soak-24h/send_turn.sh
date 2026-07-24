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
upstream_approval_required=${SOAK_UPSTREAM_APPROVAL_REQUIRED:-0}
upstream_approval_dir=${SOAK_UPSTREAM_APPROVAL_DIR:-}
upstream_approval_token=${SOAK_UPSTREAM_APPROVAL_TOKEN:-}
upstream_preview_pager=${SOAK_UPSTREAM_PREVIEW_PAGER:-auto}

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
case "$upstream_approval_required" in
  0|1) ;;
  *)
    echo "SOAK_UPSTREAM_APPROVAL_REQUIRED must be 0 or 1" >&2
    exit 64
    ;;
esac
case "$upstream_preview_pager" in
  auto|0|1) ;;
  *)
    echo "SOAK_UPSTREAM_PREVIEW_PAGER must be auto, 0, or 1" >&2
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
if [[ $upstream_approval_required == 1 ]]; then
  if [[ $stream != true ]]; then
    echo "SOAK_UPSTREAM_APPROVAL_REQUIRED=1 requires SOAK_STREAM=true" >&2
    exit 64
  fi
  if [[ $confirm_before_send == 1 ]]; then
    echo "use upstream approval or prepared downstream confirmation, not both" >&2
    exit 64
  fi
  if [[ -z $upstream_approval_dir || ! -d $upstream_approval_dir || ! -w $upstream_approval_dir ]]; then
    echo "SOAK_UPSTREAM_APPROVAL_DIR must be an existing writable shared directory" >&2
    exit 73
  fi
  if [[ -z $upstream_approval_token ]]; then
    echo "SOAK_UPSTREAM_APPROVAL_TOKEN is required when upstream approval is enabled" >&2
    exit 64
  fi
fi

mkdir -p "$output_dir"/{control,requests,responses,headers,state,tmp,upstream-previews}
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
cleanup_files=("$tmp_history")
curl_pid=""
active_reject_path=""

write_upstream_approval_marker() {
  local target_path=$1
  local decision=$2
  local marker_tmp
  marker_tmp=$(mktemp "$upstream_approval_dir/.runner-marker.XXXXXX")
  printf '%s\n' "$decision" >"$marker_tmp"
  chmod 600 "$marker_tmp"
  mv -f -- "$marker_tmp" "$target_path"
}

cleanup_send_turn() {
  local rc=$?
  if [[ -n $active_reject_path && ! -e $active_reject_path ]]; then
    write_upstream_approval_marker "$active_reject_path" "runner-exited" || true
  fi
  if [[ -n $curl_pid ]] && kill -0 "$curl_pid" >/dev/null 2>&1; then
    kill "$curl_pid" >/dev/null 2>&1 || true
    wait "$curl_pid" >/dev/null 2>&1 || true
  fi
  rm -f -- "${cleanup_files[@]}"
  exit "$rc"
}

trap cleanup_send_turn EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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
if [[ $upstream_approval_required == 1 ]]; then
  curl_headers+=(
    --header "X-Sub2API-Upstream-Approval-ID: $request_id"
    --header "X-Sub2API-Upstream-Approval-Token: $upstream_approval_token"
  )
fi
if [[ $stream == true && $turn_number == 1 ]]; then
  echo "session startup profile: session=$session_name asynchronous_companions synthetic_delay=0s"
fi

check_confirmation_safety_guards() {
  if [[ -f "$output_dir/control/STOP" ]]; then
    echo "STOP was requested while awaiting confirmation; upstream request was not sent" >&2
    return 75
  fi
  if [[ ${SOAK_DEADLINE_EPOCH:-} =~ ^[0-9]+$ ]] &&
    (( $(date +%s) >= SOAK_DEADLINE_EPOCH )); then
    echo "the run deadline passed while awaiting confirmation; upstream request was not sent" >&2
    return 75
  fi
}

read_send_request_confirmation() {
  local confirmation=""
  printf 'Type exactly SEND REQUEST to release this upstream stage: '
  if (( confirm_timeout_seconds == 0 )); then
    if ! IFS= read -r confirmation; then
      printf '\nconfirmation input closed; upstream request was not sent\n' >&2
      return 75
    fi
  elif ! IFS= read -r -t "$confirm_timeout_seconds" confirmation; then
    printf '\nconfirmation timed out or input closed; upstream request was not sent\n' >&2
    return 75
  fi
  if [[ $confirmation != "SEND REQUEST" ]]; then
    echo "confirmation did not match; upstream request was not sent" >&2
    return 75
  fi
  check_confirmation_safety_guards
}

show_upstream_preview() {
  local preview_file=$1
  local audit_file=$2
  local stage_id=$3
  local kinds=$4
  local use_pager=0

  printf '\n========== FINAL UPSTREAM REQUEST(S): NOT SENT ==========\n'
  printf 'stage: %s\n' "$stage_id"
  printf 'bundle: %s\n' "$kinds"
  printf 'audit copy: %s\n' "$audit_file"
  printf 'Authorization/x-api-key values are redacted; URL, other headers, and JSON body are final.\n'

  if [[ $upstream_preview_pager == 1 ]] ||
    [[ $upstream_preview_pager == auto && -t 0 && -t 1 && -r /dev/tty ]] &&
      command -v less >/dev/null 2>&1; then
    use_pager=1
  fi
  if (( use_pager == 1 )); then
    printf 'Opening the complete preview in less; scroll with arrows/PageUp/PageDown, then press q.\n'
    less -R "$audit_file" </dev/tty >/dev/tty
  else
    jq . "$preview_file"
  fi
  printf '========== END FINAL UPSTREAM REQUEST(S) ==========\n\n'
}

adjust_cache_expectation_after_confirmation() {
  local last_finished
  if [[ $expected_cache == hit && -f $manifest_file ]]; then
    last_finished=$(awk -F '\t' -v session="$session_name" \
      'NR > 1 && $4 == session { value=$3 } END { print value }' "$manifest_file")
    if [[ $last_finished =~ ^[0-9]+$ ]] &&
      (( $(date +%s) - last_finished >= cache_ttl_seconds )); then
      expected_cache=ttl_miss
      echo "cache expectation adjusted after confirmation: hit -> ttl_miss"
    fi
  fi
}

process_upstream_preview() {
  local preview_file=$1
  local preview_name stage_id audit_file actual_kinds expected_kinds
  preview_name=$(basename -- "$preview_file")
  stage_id=${preview_name%.preview.json}
  audit_file="$output_dir/upstream-previews/$preview_name"

  active_reject_path="$upstream_approval_dir/$stage_id.reject"
  if ! jq -e --arg approval_id "$request_id" --arg stage_id "$stage_id" '
    .version == 1
    and .approval_id == $approval_id
    and .stage_id == $stage_id
    and .network_sent == false
    and (.requests | type == "array" and length > 0)
    and all(.requests[];
      (.kind | type == "string")
      and .method == "POST"
      and (.url | startswith("https://api.anthropic.com/"))
      and (.headers | type == "array")
      and (.body_sha256 | test("^[a-f0-9]{64}$"))
      and (.content_length | type == "number" and . > 0)
      and (.body | type == "object")
      and any(.headers[];
        (.name | ascii_downcase) == "authorization"
        and any(.values[]; contains("[redacted]"))
      )
    )
    and all(
      .requests[].headers[]
      | select(
          (.name | ascii_downcase) == "authorization"
          or (.name | ascii_downcase) == "x-api-key"
          or (.name | ascii_downcase) == "x-sub2api-upstream-approval-token"
        );
      all(.values[]; contains("[redacted]"))
    )
  ' "$preview_file" >/dev/null; then
    echo "invalid or unsafe upstream preview: $preview_file" >&2
    write_upstream_approval_marker "$active_reject_path" "invalid-preview"
    active_reject_path=""
    return 75
  fi

  actual_kinds=$(jq -r '[.requests[].kind] | join(",")' "$preview_file")
  if (( processed_approval_stages == 0 && turn_number == 1 )); then
    expected_kinds="quota,title,main"
  else
    expected_kinds="main"
  fi
  if [[ $actual_kinds != "$expected_kinds" ]]; then
    echo "unexpected upstream bundle for $stage_id: expected=$expected_kinds actual=$actual_kinds" >&2
    write_upstream_approval_marker "$active_reject_path" "unexpected-bundle"
    active_reject_path=""
    return 75
  fi

  cp -- "$preview_file" "$audit_file"
  show_upstream_preview "$preview_file" "$audit_file" "$stage_id" "$actual_kinds"
  if ! check_confirmation_safety_guards || ! read_send_request_confirmation; then
    write_upstream_approval_marker "$active_reject_path" "rejected"
    active_reject_path=""
    return 75
  fi
  write_upstream_approval_marker "$upstream_approval_dir/$stage_id.approve" "approved"
  active_reject_path=""
  processed_approval_stages=$((processed_approval_stages + 1))
  if (( processed_approval_stages == 1 )); then
    adjust_cache_expectation_after_confirmation
  fi
  echo "confirmation accepted; releasing exact upstream stage=$stage_id bundle=$actual_kinds"
}

monitor_upstream_approvals() {
  local preview_file preview_name
  while :; do
    preview_file=$(
      find "$upstream_approval_dir" -maxdepth 1 -type f \
        -name "$request_id-s*.preview.json" -print 2>/dev/null |
        sort |
        while IFS= read -r candidate; do
          preview_name=$(basename -- "$candidate")
          if [[ ! -f "$output_dir/upstream-previews/$preview_name" ]]; then
            printf '%s\n' "$candidate"
            break
          fi
        done
    )
    if [[ -n $preview_file ]]; then
      process_upstream_preview "$preview_file" || return $?
      continue
    fi
    if [[ -f $curl_status_file ]]; then
      break
    fi
    sleep 0.1
  done
  if (( processed_approval_stages == 0 )); then
    echo "the downstream request ended before the server produced an upstream preview" >&2
    return 75
  fi
}

started_epoch=$(date +%s)
curl_metrics=""
curl_exit=0
if [[ $upstream_approval_required == 1 ]]; then
  if find "$upstream_approval_dir" -maxdepth 1 \
    -name "$request_id-s*" -print -quit 2>/dev/null | grep -q .; then
    echo "stale upstream approval files already exist for request ID $request_id" >&2
    exit 73
  fi
  curl_metrics_file=$(mktemp "$output_dir/tmp/curl-metrics.XXXXXX")
  curl_status_file=$(mktemp "$output_dir/tmp/curl-status.XXXXXX")
  rm -f -- "$curl_status_file"
  cleanup_files+=("$curl_metrics_file" "$curl_status_file")
  processed_approval_stages=0
  (
    set +e
    metrics=$(curl \
      --silent \
      --show-error \
      --connect-timeout 15 \
      --max-time 0 \
      --request POST "$endpoint" \
      "${curl_headers[@]}" \
      --data-binary "@$request_file" \
      --dump-header "$header_file" \
      --output "$wire_response_file" \
      --write-out $'%{http_code}\t%{time_total}')
    child_rc=$?
    printf '%s' "$metrics" >"$curl_metrics_file"
    printf '%s\n' "$child_rc" >"$curl_status_file"
  ) &
  curl_pid=$!

  approval_monitor_rc=0
  monitor_upstream_approvals || approval_monitor_rc=$?
  if (( approval_monitor_rc != 0 )); then
    for _ in {1..100}; do
      [[ -f $curl_status_file ]] && break
      sleep 0.1
    done
    if [[ ! -f $curl_status_file ]]; then
      kill "$curl_pid" >/dev/null 2>&1 || true
    fi
    wait "$curl_pid" >/dev/null 2>&1 || true
    curl_pid=""
    exit "$approval_monitor_rc"
  fi
  wait "$curl_pid" || true
  curl_pid=""
  curl_metrics=$(<"$curl_metrics_file")
  curl_exit=$(<"$curl_status_file")
else
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
fi
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
cleanup_files+=("$next_state")
jq -n \
  --slurpfile history "$tmp_history" \
  --slurpfile response "$response_file" \
  --rawfile prompt "$prompt_file" \
  '$history[0]
   + [{role: "user", content: [{type: "text", text: $prompt}]}]
   + [{role: "assistant", content: $response[0].content}]' >"$next_state"
mv "$next_state" "$state_file"

echo "ok session=$session_name turn=$turn_number expected=$expected_cache http=$http_code cache_create=$cache_creation cache_read=$cache_read response=$response_file"
