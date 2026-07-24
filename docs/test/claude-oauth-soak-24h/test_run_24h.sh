#!/usr/bin/env bash
set -euo pipefail

umask 077

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-soak-runner.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT

fake_bin="$test_dir/bin"
output_dir="$test_dir/run"
approval_dir="$test_dir/upstream-approval"
mkdir -p "$fake_bin" "$approval_dir"
cp -- "$script_dir/testdata/fake_curl.sh" "$fake_bin/curl"
chmod 700 "$fake_bin/curl"

set +e
for ((confirmation_number = 1; confirmation_number <= 30; confirmation_number++)); do
  printf '%s\n' 'SEND REQUEST'
done | \
  PATH="$fake_bin:$PATH" \
  SOAK_BASE_URL='http://127.0.0.1:1' \
  SOAK_API_KEY='test-key' \
  SOAK_ACCOUNT_ID='1' \
  SOAK_OUTPUT_DIR="$output_dir" \
  SOAK_IDENTITY_HOME="$test_dir/identity" \
  SOAK_USAGE_GUARD_MODE='off' \
  SOAK_SKIP_WAITS='1' \
  SOAK_STREAM='true' \
  SOAK_CONFIRM_BEFORE_SEND='0' \
  SOAK_UPSTREAM_APPROVAL_DIR="$approval_dir" \
  SOAK_UPSTREAM_APPROVAL_TOKEN='approval-secret' \
  SOAK_UPSTREAM_PREVIEW_PAGER='0' \
  FAKE_CURL_UPSTREAM_APPROVAL_DIR="$approval_dir" \
  FAKE_CURL_EXPECT_APPROVAL_TOKEN='approval-secret' \
  FAKE_CURL_RESPONSE_FILE="$script_dir/testdata/valid-response.json" \
    "$script_dir/run_24h.sh" >"$test_dir/run.log" 2>&1
runner_rc=$?
set -e

if (( runner_rc != 0 )); then
  echo "the confirmed single-session runner failed with exit code $runner_rc" >&2
  sed -n '1,160p' "$test_dir/run.log" >&2
  exit 1
fi

request_count=$(find "$output_dir/requests" -type f -name '*.json' | wc -l | tr -d ' ')
preview_count=$(find "$output_dir/upstream-previews" -type f -name '*.preview.json' | wc -l | tr -d ' ')
prepared_count=$(grep -Fc 'FINAL UPSTREAM REQUEST(S): NOT SENT' "$test_dir/run.log")
accepted_count=$(grep -Fc 'confirmation accepted; releasing exact upstream stage=' "$test_dir/run.log")
manifest_rows=$(wc -l <"$output_dir/manifest.tsv" | tr -d ' ')
state_user_turns=$(jq '[.[] | select(.role == "user")] | length' "$output_dir/state/session-f.messages.json")

if [[ $request_count != 30 || $preview_count != 30 || $prepared_count != 30 || $accepted_count != 30 ]]; then
  echo "runner counts differ: requests=$request_count previews=$preview_count prepared=$prepared_count accepted=$accepted_count" >&2
  exit 1
fi
first_bundle_count=0
main_only_count=0
while IFS= read -r preview; do
  kinds=$(jq -r '[.requests[].kind] | join(",")' "$preview")
  case "$kinds" in
    quota,title,main) first_bundle_count=$((first_bundle_count + 1)) ;;
    main) main_only_count=$((main_only_count + 1)) ;;
    *)
      echo "unexpected upstream approval bundle: $kinds ($preview)" >&2
      exit 1
      ;;
  esac
done < <(find "$output_dir/upstream-previews" -type f -name '*.preview.json' -print)
if [[ $first_bundle_count != 1 || $main_only_count != 29 ]]; then
  echo "approval bundle counts differ: first=$first_bundle_count main_only=$main_only_count" >&2
  exit 1
fi
if [[ $manifest_rows != 31 || $state_user_turns != 30 ]]; then
  echo "runner state differs: manifest_rows=$manifest_rows state_user_turns=$state_user_turns" >&2
  exit 1
fi
if ! awk -F '\t' 'NR > 1 && ($4 != "session-f" || $5 != NR - 1) { exit 1 }' "$output_dir/manifest.tsv"; then
  echo "manifest is not a strictly ordered 30-turn session-f run" >&2
  exit 1
fi
if ! jq -e '
  .parallel_sessions == 0
  and .confirmation_required == 1
  and .confirmation_scope == "final_upstream_stage"
  and .confirmation_timeout_seconds == 0
  and .approval_mode == "manual"
  and .planned_sessions == 1
  and .planned_requests == 30
' <(
  awk -F '=' '
    {
      key=$1
      value=substr($0, index($0, "=") + 1)
      if (value ~ /^[0-9]+$/) {
        printf "%s\"%s\":%s", separator, key, value
      } else {
        gsub(/\\/, "\\\\", value)
        gsub(/"/, "\\\"", value)
        printf "%s\"%s\":\"%s\"", separator, key, value
      }
      separator=","
    }
    BEGIN { printf "{" }
    END { print "}" }
  ' "$output_dir/run.meta"
) >/dev/null; then
  echo "run.meta does not record the single-session confirmation mode" >&2
  exit 1
fi

automatic_output_dir="$test_dir/automatic-run"
automatic_approval_dir="$test_dir/automatic-upstream-approval"
mkdir -p "$automatic_approval_dir"

set +e
PATH="$fake_bin:$PATH" \
  SOAK_BASE_URL='http://127.0.0.1:1' \
  SOAK_API_KEY='test-key' \
  SOAK_ACCOUNT_ID='1' \
  SOAK_OUTPUT_DIR="$automatic_output_dir" \
  SOAK_IDENTITY_HOME="$test_dir/automatic-identity" \
  SOAK_USAGE_GUARD_MODE='off' \
  SOAK_SKIP_WAITS='1' \
  SOAK_STREAM='true' \
  SOAK_CONFIRM_BEFORE_SEND='0' \
  SOAK_AUTO_APPROVE_UPSTREAM='1' \
  SOAK_UPSTREAM_APPROVAL_DIR="$automatic_approval_dir" \
  SOAK_UPSTREAM_APPROVAL_TOKEN='approval-secret' \
  SOAK_UPSTREAM_PREVIEW_PAGER='0' \
  FAKE_CURL_UPSTREAM_APPROVAL_DIR="$automatic_approval_dir" \
  FAKE_CURL_EXPECT_APPROVAL_TOKEN='approval-secret' \
  FAKE_CURL_RESPONSE_FILE="$script_dir/testdata/valid-response.json" \
    "$script_dir/run_24h.sh" </dev/null >"$test_dir/automatic-run.log" 2>&1
automatic_runner_rc=$?
set -e

if (( automatic_runner_rc != 0 )); then
  echo "the automatic single-session runner failed with exit code $automatic_runner_rc" >&2
  sed -n '1,160p' "$test_dir/automatic-run.log" >&2
  exit 1
fi

automatic_request_count=$(
  find "$automatic_output_dir/requests" -type f -name '*.json' |
    wc -l |
    tr -d ' '
)
automatic_accepted_count=$(
  grep -Fc 'automatic upstream approval accepted; releasing exact stage=' \
    "$test_dir/automatic-run.log"
)
if [[ $automatic_request_count != 30 || $automatic_accepted_count != 30 ]] ||
  grep -F 'Type exactly SEND REQUEST' "$test_dir/automatic-run.log" >/dev/null; then
  echo "automatic runner was not a 30-request stdin-free run" >&2
  exit 1
fi

if ! jq -e '
  .parallel_sessions == 0
  and .confirmation_required == 0
  and .confirmation_scope == "final_upstream_stage"
  and .approval_mode == "automatic"
  and .scheduling_mode == "single_session_automatic_waves"
  and .planned_sessions == 1
  and .planned_requests == 30
' <(
  awk -F '=' '
    {
      key=$1
      value=substr($0, index($0, "=") + 1)
      if (value ~ /^[0-9]+$/) {
        printf "%s\"%s\":%s", separator, key, value
      } else {
        gsub(/\\/, "\\\\", value)
        gsub(/"/, "\\\"", value)
        printf "%s\"%s\":\"%s\"", separator, key, value
      }
      separator=","
    }
    BEGIN { printf "{" }
    END { print "}" }
  ' "$automatic_output_dir/run.meta"
) >/dev/null; then
  echo "run.meta does not record automatic approval mode" >&2
  exit 1
fi

printf '%s\n' 'single-session manual and automatic 30-request runner tests passed'
