#!/usr/bin/env bash
set -euo pipefail

umask 077

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-soak-send-turn.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT

fake_bin="$test_dir/bin"
mkdir -p "$fake_bin"
cp -- "$script_dir/testdata/fake_curl.sh" "$fake_bin/curl"
chmod 700 "$fake_bin/curl"

prompt_file="$test_dir/prompt.md"
identity_file="$test_dir/session-identities.json"
printf '%s\n' 'Please review this implementation and explain the main correctness risks.' >"$prompt_file"
printf '%s\n' \
  '{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sessions":{"session-a":{"session_id":"11111111-1111-4111-8111-111111111111"}}}' \
  >"$identity_file"

valid_response="$test_dir/valid-response.json"
truncated_response="$test_dir/truncated-response.json"
thinking_only_response="$test_dir/thinking-only-response.json"
tool_use_response="$test_dir/tool-use-response.json"
stream_response="$test_dir/stream-response.sse"

printf '%s\n' \
  '{"id":"msg_valid","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signed-thinking"},{"type":"text","text":"The primary risk is a lost update during concurrent retries."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":200}}' \
  >"$valid_response"
printf '%s\n' \
  '{"id":"msg_truncated","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signed-thinking"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1536}}' \
  >"$truncated_response"
printf '%s\n' \
  '{"id":"msg_no_text","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signed-thinking"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":100}}' \
  >"$thinking_only_response"
printf '%s\n' \
  '{"id":"msg_tool","type":"message","role":"assistant","content":[{"type":"text","text":"I will inspect the file."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"example.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":100}}' \
  >"$tool_use_response"
printf '%s\n' \
  'event: message_start' \
  'data: {"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":0,"cache_creation_input_tokens":9000,"cache_read_input_tokens":0}}}' \
  '' \
  'event: content_block_start' \
  'data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}' \
  '' \
  'event: content_block_delta' \
  'data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed-stream-thinking"}}' \
  '' \
  'event: content_block_stop' \
  'data: {"type":"content_block_stop","index":0}' \
  '' \
  'event: content_block_start' \
  'data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}' \
  '' \
  'event: content_block_delta' \
  'data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"The primary risk is "}}' \
  '' \
  'event: content_block_delta' \
  'data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"a lost update."}}' \
  '' \
  'event: content_block_stop' \
  'data: {"type":"content_block_stop","index":1}' \
  '' \
  'event: message_delta' \
  'data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}' \
  '' \
  'event: message_stop' \
  'data: {"type":"message_stop"}' \
  '' >"$stream_response"

export PATH="$fake_bin:$PATH"
export SOAK_BASE_URL='http://127.0.0.1:1'
export SOAK_API_KEY='test-key'
export SOAK_MODEL='claude-opus-4-8'
export SOAK_SESSION_IDENTITIES_FILE="$identity_file"
export SOAK_PARALLEL_SESSIONS=0
export SOAK_SKIP_WAITS=1
unset SOAK_UPSTREAM_APPROVAL_REQUIRED SOAK_UPSTREAM_APPROVAL_DIR SOAK_UPSTREAM_APPROVAL_TOKEN
unset FAKE_CURL_UPSTREAM_APPROVAL_DIR FAKE_CURL_EXPECT_APPROVAL_TOKEN

latest_request_file() {
  find "$1/requests" -type f -name '*.json' -print | sort | tail -n 1
}

run_success_case() {
  local name=$1
  local response_file=$2
  local content_type=${3:-application/json}
  local output_dir="$test_dir/$name"
  export SOAK_OUTPUT_DIR=$output_dir
  export FAKE_CURL_RESPONSE_FILE=$response_file
  export FAKE_CURL_CONTENT_TYPE=$content_type
  "$script_dir/send_turn.sh" session-a "$prompt_file" cold >"$test_dir/$name.log" 2>&1
  printf '%s\n' "$output_dir"
}

run_failure_case() {
  local name=$1
  local response_file=$2
  local expected_message=$3
  local output_dir="$test_dir/$name"
  local rc
  export SOAK_OUTPUT_DIR=$output_dir
  export FAKE_CURL_RESPONSE_FILE=$response_file
  export FAKE_CURL_CONTENT_TYPE=application/json
  set +e
  "$script_dir/send_turn.sh" session-a "$prompt_file" cold >"$test_dir/$name.log" 2>&1
  rc=$?
  set -e
  if (( rc == 0 )); then
    echo "$name unexpectedly succeeded" >&2
    exit 1
  fi
  if [[ -e "$output_dir/state/session-a.messages.json" ]]; then
    echo "$name advanced session state after a rejected response" >&2
    exit 1
  fi
  if ! grep -F "$expected_message" "$test_dir/$name.log" >/dev/null; then
    echo "$name did not report the expected validation error" >&2
    sed -n '1,80p' "$test_dir/$name.log" >&2
    exit 1
  fi
  if [[ $(wc -l <"$output_dir/manifest.tsv" | tr -d ' ') != 2 ]]; then
    echo "$name did not retain one manifest audit row" >&2
    exit 1
  fi
}

unset SOAK_MAX_TOKENS
unset SOAK_STREAM
default_output=$(run_success_case default-limit "$valid_response")
default_request=$(latest_request_file "$default_output")
jq -e 'has("max_tokens") | not' "$default_request" >/dev/null
jq -e '.stream == false' "$default_request" >/dev/null
jq -e '.messages[-1].content[0].cache_control == {type:"ephemeral",ttl:"5m"}' "$default_request" >/dev/null
awk -F '\t' 'NR == 2 { exit !($14 == "false" && $15 == "0") }' "$default_output/manifest.tsv"
jq -e --slurpfile response "$valid_response" '
  ((.[-2].content[0] | has("cache_control")) | not)
  and (.[-1].content == $response[0].content)
' "$default_output/state/session-a.messages.json" >/dev/null

export SOAK_MAX_TOKENS=8192
explicit_output=$(run_success_case explicit-limit "$valid_response")
explicit_request=$(latest_request_file "$explicit_output")
jq -e '.max_tokens == 8192' "$explicit_request" >/dev/null

unset SOAK_MAX_TOKENS
export SOAK_STREAM=true
export SOAK_COMPANION_DELAY_MIN_SECONDS=5
export SOAK_COMPANION_DELAY_MAX_SECONDS=55
stream_curl_args="$test_dir/streaming-curl-args.txt"
export FAKE_CURL_ARGS_FILE=$stream_curl_args
stream_output=$(run_success_case streaming "$stream_response" text/event-stream)
stream_request=$(latest_request_file "$stream_output")
jq -e '.stream == true' "$stream_request" >/dev/null
jq -e '
  .id == "msg_stream"
  and .stop_reason == "end_turn"
  and .usage.output_tokens == 42
  and .usage.cache_creation_input_tokens == 9000
  and .content == [
    {type:"thinking",thinking:"",signature:"signed-stream-thinking"},
    {type:"text",text:"The primary risk is a lost update."}
  ]
' "$stream_output/responses/$(basename "$stream_request")" >/dev/null
jq -e '
  .[-1].content == [
    {type:"thinking",thinking:"",signature:"signed-stream-thinking"},
    {type:"text",text:"The primary risk is a lost update."}
  ]
' "$stream_output/state/session-a.messages.json" >/dev/null
awk -F '\t' 'NR == 2 { exit !($14 == "true" && $15 == "0") }' "$stream_output/manifest.tsv"
if grep -Fi 'X-Sub2API-Claude-Companion-Delay-Seconds:' "$stream_curl_args" >/dev/null; then
  echo "streaming request outside approval mode leaked the private companion delay header" >&2
  exit 1
fi
unset SOAK_STREAM SOAK_COMPANION_DELAY_MIN_SECONDS SOAK_COMPANION_DELAY_MAX_SECONDS
unset FAKE_CURL_ARGS_FILE

run_failure_case truncated "$truncated_response" 'response was truncated at max_tokens'
run_failure_case no-visible-text "$thinking_only_response" 'end_turn response has no non-empty text block'
run_failure_case unexpected-tool-use "$tool_use_response" 'unexpected stop_reason=tool_use'

confirmation_reject_output="$test_dir/confirmation-reject"
confirmation_reject_called="$test_dir/confirmation-reject.called"
export SOAK_OUTPUT_DIR=$confirmation_reject_output
export FAKE_CURL_RESPONSE_FILE=$valid_response
export FAKE_CURL_CALLED_FILE=$confirmation_reject_called
unset FAKE_CURL_CAPTURE_REQUEST_FILE
set +e
printf '%s\n' 'DO NOT SEND' | \
  SOAK_CONFIRM_BEFORE_SEND=1 SOAK_CONFIRM_TIMEOUT_SECONDS=5 \
  "$script_dir/send_turn.sh" session-a "$prompt_file" cold \
  >"$test_dir/confirmation-reject.log" 2>&1
confirmation_reject_rc=$?
set -e
if (( confirmation_reject_rc != 75 )); then
  echo "confirmation rejection returned $confirmation_reject_rc instead of 75" >&2
  exit 1
fi
if [[ -e $confirmation_reject_called || -e "$confirmation_reject_output/state/session-a.messages.json" ]]; then
  echo "a rejected confirmation sent the request or advanced state" >&2
  exit 1
fi
if [[ $(find "$confirmation_reject_output/requests" -type f -name '*.json' | wc -l | tr -d ' ') != 1 ]]; then
  echo "confirmation rejection did not retain exactly one prepared request" >&2
  exit 1
fi

confirmation_deadline_output="$test_dir/confirmation-deadline"
confirmation_deadline_called="$test_dir/confirmation-deadline.called"
export SOAK_OUTPUT_DIR=$confirmation_deadline_output
export FAKE_CURL_CALLED_FILE=$confirmation_deadline_called
set +e
printf '%s\n' 'SEND REQUEST' | \
  SOAK_CONFIRM_BEFORE_SEND=1 SOAK_CONFIRM_TIMEOUT_SECONDS=0 SOAK_DEADLINE_EPOCH=1 \
  "$script_dir/send_turn.sh" session-a "$prompt_file" cold \
  >"$test_dir/confirmation-deadline.log" 2>&1
confirmation_deadline_rc=$?
set -e
if (( confirmation_deadline_rc != 75 )); then
  echo "expired confirmation returned $confirmation_deadline_rc instead of 75" >&2
  exit 1
fi
if [[ -e $confirmation_deadline_called || -e "$confirmation_deadline_output/state/session-a.messages.json" ]]; then
  echo "a confirmation accepted after the deadline sent the request or advanced state" >&2
  exit 1
fi

confirmation_accept_output="$test_dir/confirmation-accept"
confirmation_accept_called="$test_dir/confirmation-accept.called"
confirmation_capture="$test_dir/confirmation-captured-request.json"
export SOAK_OUTPUT_DIR=$confirmation_accept_output
export FAKE_CURL_CALLED_FILE=$confirmation_accept_called
export FAKE_CURL_CAPTURE_REQUEST_FILE=$confirmation_capture
unset SOAK_DEADLINE_EPOCH
printf '%s\n' 'SEND REQUEST' | \
  SOAK_CONFIRM_BEFORE_SEND=1 SOAK_CONFIRM_TIMEOUT_SECONDS=0 \
  "$script_dir/send_turn.sh" session-a "$prompt_file" cold \
  >"$test_dir/confirmation-accept.log" 2>&1
confirmation_request=$(latest_request_file "$confirmation_accept_output")
if [[ ! -f $confirmation_accept_called || ! -f "$confirmation_accept_output/state/session-a.messages.json" ]]; then
  echo "an accepted confirmation did not send the request or advance state" >&2
  exit 1
fi
if ! cmp -s "$confirmation_request" "$confirmation_capture"; then
  echo "the confirmed request differed from the prepared request file" >&2
  exit 1
fi

upstream_approval_dir="$test_dir/upstream-approval"
upstream_approval_output="$test_dir/upstream-approval-accept"
upstream_approval_curl_args="$test_dir/upstream-approval-curl-args.txt"
mkdir -p "$upstream_approval_dir"
printf '%s\n' 'SEND REQUEST' | \
  SOAK_OUTPUT_DIR="$upstream_approval_output" \
  SOAK_STREAM=true \
  SOAK_COMPANION_DELAY_MIN_SECONDS=37 \
  SOAK_COMPANION_DELAY_MAX_SECONDS=37 \
  SOAK_CONFIRM_BEFORE_SEND=0 \
  SOAK_UPSTREAM_APPROVAL_REQUIRED=1 \
  SOAK_UPSTREAM_APPROVAL_DIR="$upstream_approval_dir" \
  SOAK_UPSTREAM_APPROVAL_TOKEN='approval-secret' \
  SOAK_UPSTREAM_PREVIEW_PAGER=0 \
  FAKE_CURL_UPSTREAM_APPROVAL_DIR="$upstream_approval_dir" \
  FAKE_CURL_EXPECT_APPROVAL_TOKEN='approval-secret' \
  FAKE_CURL_ARGS_FILE="$upstream_approval_curl_args" \
  FAKE_CURL_RESPONSE_FILE="$valid_response" \
  "$script_dir/send_turn.sh" session-a "$prompt_file" cold \
  >"$test_dir/upstream-approval-accept.log" 2>&1

upstream_preview=$(find "$upstream_approval_output/upstream-previews" -type f -name '*.preview.json' -print -quit)
if [[ -z $upstream_preview ]]; then
  echo "accepted upstream approval did not retain its audit preview" >&2
  exit 1
fi
jq -e '
  .network_sent == false
  and [.requests[].kind] == ["quota", "title", "main"]
  and all(.requests[]; .url == "https://api.anthropic.com/v1/messages?beta=true")
' "$upstream_preview" >/dev/null
if ! grep -F 'FINAL UPSTREAM REQUEST(S): NOT SENT' "$test_dir/upstream-approval-accept.log" >/dev/null ||
  grep -F 'PREPARED REQUEST: NOT SENT' "$test_dir/upstream-approval-accept.log" >/dev/null; then
  echo "upstream approval displayed the wrong request layer" >&2
  exit 1
fi
if [[ $(find "$upstream_approval_dir" -type f -name '*.approve' | wc -l | tr -d ' ') != 1 ]]; then
  echo "accepted upstream approval did not release exactly one stage" >&2
  exit 1
fi
if ! grep -Fx 'X-Sub2API-Claude-Companion-Delay-Seconds: 37' "$upstream_approval_curl_args" >/dev/null ||
  ! grep -F 'approved send order: quota -> wait 37s -> title -> main' "$test_dir/upstream-approval-accept.log" >/dev/null; then
  echo "accepted upstream approval did not carry and display the exact companion delay" >&2
  exit 1
fi
awk -F '\t' 'NR == 2 { exit !($14 == "true" && $15 == "37") }' \
  "$upstream_approval_output/manifest.tsv"

upstream_reject_dir="$test_dir/upstream-reject"
upstream_reject_output="$test_dir/upstream-approval-reject"
mkdir -p "$upstream_reject_dir"
set +e
printf '%s\n' 'DO NOT SEND' | \
  SOAK_OUTPUT_DIR="$upstream_reject_output" \
  SOAK_STREAM=true \
  SOAK_CONFIRM_BEFORE_SEND=0 \
  SOAK_UPSTREAM_APPROVAL_REQUIRED=1 \
  SOAK_UPSTREAM_APPROVAL_DIR="$upstream_reject_dir" \
  SOAK_UPSTREAM_APPROVAL_TOKEN='approval-secret' \
  SOAK_UPSTREAM_PREVIEW_PAGER=0 \
  FAKE_CURL_UPSTREAM_APPROVAL_DIR="$upstream_reject_dir" \
  FAKE_CURL_EXPECT_APPROVAL_TOKEN='approval-secret' \
  FAKE_CURL_RESPONSE_FILE="$valid_response" \
  "$script_dir/send_turn.sh" session-a "$prompt_file" cold \
  >"$test_dir/upstream-approval-reject.log" 2>&1
upstream_reject_rc=$?
set -e
if (( upstream_reject_rc != 75 )); then
  echo "upstream approval rejection returned $upstream_reject_rc instead of 75" >&2
  exit 1
fi
if [[ $(find "$upstream_reject_dir" -type f -name '*.reject' | wc -l | tr -d ' ') != 1 ||
  -e "$upstream_reject_output/state/session-a.messages.json" ]]; then
  echo "rejected upstream approval did not remain blocked before state advancement" >&2
  exit 1
fi

printf '%s\n' 'send_turn response validation tests passed'
