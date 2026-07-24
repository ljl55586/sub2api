#!/usr/bin/env bash
set -euo pipefail

umask 077

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-cache-demo.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT

fake_bin="$test_dir/bin"
sequence_dir="$test_dir/responses"
counter_file="$test_dir/curl-count"
captured_request="$test_dir/last-request.json"
demo_output="$test_dir/demo-output"
mkdir -p "$fake_bin" "$sequence_dir"
cp -- "$script_dir/testdata/fake_curl.sh" "$fake_bin/curl"
chmod 700 "$fake_bin/curl"

printf '%s\n' \
  '{"id":"msg_first","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"first-signature"},{"type":"text","text":"The safest design uses an immutable ledger and idempotent capture commands."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":200,"cache_creation_input_tokens":12000,"cache_read_input_tokens":0}}' \
  >"$sequence_dir/1.json"
printf '%s\n' \
  '{"id":"msg_second","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"second-signature"},{"type":"text","text":"The three highest-risk timelines are duplicate capture, refund overlap, and commit ambiguity."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":220,"cache_creation_input_tokens":500,"cache_read_input_tokens":12000}}' \
  >"$sequence_dir/2.json"

export PATH="$fake_bin:$PATH"
export SOAK_BASE_URL='http://127.0.0.1:1'
export SOAK_API_KEY='test-key'
export SOAK_ACCOUNT_ID=1
export SOAK_IDENTITY_HOME="$test_dir/identity"
export CACHE_DEMO_OUTPUT_DIR=$demo_output
export CACHE_DEMO_CONFIRM_TIMEOUT_SECONDS=5
export FAKE_CURL_SEQUENCE_DIR=$sequence_dir
export FAKE_CURL_SEQUENCE_COUNTER_FILE=$counter_file
export FAKE_CURL_CAPTURE_REQUEST_FILE=$captured_request
unset SOAK_MAX_TOKENS FAKE_CURL_RESPONSE_FILE

printf '%s\n' 'SEND REQUEST' | \
  "$script_dir/cache_hit_demo.sh" >"$test_dir/demo.log" 2>&1

if [[ $(<"$counter_file") != 2 ]]; then
  echo "cache demo did not make exactly two curl calls" >&2
  exit 1
fi
if ! grep -F 'CACHE HIT CONFIRMED' "$test_dir/demo.log" >/dev/null; then
  echo "cache demo did not report the expected cache hit" >&2
  sed -n '1,120p' "$test_dir/demo.log" >&2
  exit 1
fi
if [[ $(wc -l <"$demo_output/manifest.tsv" | tr -d ' ') != 3 ]]; then
  echo "cache demo manifest does not contain exactly two requests" >&2
  exit 1
fi

second_request=$(find "$demo_output/requests" -type f -name '*-cache-demo-t2-*.json' -print | sort | tail -n 1)
if [[ -z $second_request ]]; then
  echo "cache demo did not retain the prepared second request" >&2
  exit 1
fi
if ! cmp -s "$second_request" "$captured_request"; then
  echo "cache demo sent a different body than the prepared second request" >&2
  exit 1
fi
jq -e '
  (has("max_tokens") | not)
  and (.messages | length == 3)
  and ([.messages[].role] == ["user","assistant","user"])
  and ((.messages[0].content[0] | has("cache_control")) | not)
  and (.messages[1].content == [
    {"type":"thinking","thinking":"","signature":"first-signature"},
    {"type":"text","text":"The safest design uses an immutable ledger and idempotent capture commands."}
  ])
  and (.messages[2].content[0].cache_control == {"type":"ephemeral","ttl":"5m"})
' "$second_request" >/dev/null

first_metadata=$(find "$demo_output/requests" -type f -name '*-cache-demo-t1-*.json' -print | sort | head -n 1)
if [[ $(jq -r '.metadata.user_id' "$first_metadata") != $(jq -r '.metadata.user_id' "$second_request") ]]; then
  echo "cache demo changed metadata.user_id between requests" >&2
  exit 1
fi

printf '%s\n' 'cache hit confirmation demo tests passed'
