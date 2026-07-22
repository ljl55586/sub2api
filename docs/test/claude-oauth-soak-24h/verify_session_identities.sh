#!/usr/bin/env bash
set -euo pipefail

run_dir=${1:-${SOAK_OUTPUT_DIR:-}}
if [[ -z $run_dir ]]; then
  echo "usage: $0 <run-output-directory>" >&2
  exit 64
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
identity_file=${SOAK_SESSION_IDENTITIES_FILE:-"$run_dir/session-identities.json"}
if [[ ! -f $identity_file ]]; then
  echo "session identity file not found: $identity_file" >&2
  exit 66
fi

if ! jq -e '
  .schema_version == 1
  and (.account_id | type == "string" and test("^[1-9][0-9]*$"))
  and (.device_id | test("^[a-f0-9]{64}$"))
  and (.sessions | type == "object" and length > 0)
  and (all(.sessions[].session_id; test("^[a-f0-9]{8}-[a-f0-9]{4}-[1-5][a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$")))
  and (([.sessions[].session_id] | length) == ([.sessions[].session_id] | unique | length))
' "$identity_file" >/dev/null; then
  echo "invalid or duplicate session identities: $identity_file" >&2
  exit 65
fi

scheduled_sessions=$(awk -F '\t' '$1 !~ /^#/ && $3 != "" && !seen[$3]++ { print $3 }' "$script_dir/schedule.tsv")
for session_name in $scheduled_sessions; do
  if ! jq -e --arg session "$session_name" '.sessions[$session].session_id | type == "string"' "$identity_file" >/dev/null; then
    echo "scheduled session identity is missing: $session_name" >&2
    exit 65
  fi
done

request_count=0
if [[ -d $run_dir/requests ]]; then
  for session_name in $scheduled_sessions; do
    expected_session_id=$(jq -er --arg session "$session_name" '.sessions[$session].session_id' "$identity_file")
    expected_device_id=$(jq -er '.device_id' "$identity_file")
    for request_file in "$run_dir"/requests/*-"$session_name"-t*.json; do
      [[ -e $request_file ]] || continue
      request_count=$((request_count + 1))
      actual_device_id=$(jq -er '.metadata.user_id | fromjson | .device_id' "$request_file")
      actual_session_id=$(jq -er '.metadata.user_id | fromjson | .session_id' "$request_file")
      if [[ $actual_device_id != "$expected_device_id" || $actual_session_id != "$expected_session_id" ]]; then
        echo "request identity mismatch: $request_file" >&2
        exit 65
      fi
    done
  done
fi

jq -r '
  "account_id=\(.account_id)",
  "device_id=\(.device_id)",
  (.sessions | to_entries[] | "\(.key)=\(.value.session_id)")
' "$identity_file"
printf 'verified unique identities and %s request bodies\n' "$request_count"
