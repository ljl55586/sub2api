#!/usr/bin/env bash
set -euo pipefail

umask 077

if (( $# == 0 )); then
  echo "usage: $0 <session-name> [session-name ...]" >&2
  exit 64
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
: "${SOAK_OUTPUT_DIR:?set SOAK_OUTPUT_DIR before initializing session identities}"
: "${SOAK_ACCOUNT_ID:?set SOAK_ACCOUNT_ID to the Claude account ID}"

if [[ ! $SOAK_ACCOUNT_ID =~ ^[1-9][0-9]*$ ]]; then
  echo "SOAK_ACCOUNT_ID must be a positive integer" >&2
  exit 64
fi

for command_name in jq od tr; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "required command not found: $command_name" >&2
    exit 69
  fi
done

for session_name in "$@"; do
  if [[ ! $session_name =~ ^[A-Za-z0-9._-]+$ ]]; then
    echo "invalid session name: $session_name" >&2
    exit 64
  fi
done

identity_home=${SOAK_IDENTITY_HOME:-"$script_dir/.runtime-identity"}
device_id_file=${SOAK_DEVICE_ID_FILE:-"$identity_home/account-${SOAK_ACCOUNT_ID}.device-id"}
identity_file=${SOAK_SESSION_IDENTITIES_FILE:-"$SOAK_OUTPUT_DIR/session-identities.json"}

mkdir -p "$SOAK_OUTPUT_DIR/control" "$(dirname -- "$device_id_file")" "$(dirname -- "$identity_file")"

exec 7>>"$SOAK_OUTPUT_DIR/control/session-identities.lock"
if command -v flock >/dev/null 2>&1; then
  flock -x 7
elif [[ ${SOAK_PARALLEL_SESSIONS:-0} == 1 ]]; then
  echo "flock is required to initialize identities in parallel mode" >&2
  exit 69
fi

random_hex() {
  local byte_count=$1
  LC_ALL=C od -An -N "$byte_count" -tx1 /dev/urandom | tr -d ' \n'
}

new_uuid() {
  local raw
  if [[ -r /proc/sys/kernel/random/uuid ]]; then
    tr '[:upper:]' '[:lower:]' </proc/sys/kernel/random/uuid
    return
  fi
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
    return
  fi
  raw=$(random_hex 16)
  printf '%s-%s-4%s-8%s-%s\n' \
    "${raw:0:8}" "${raw:8:4}" "${raw:13:3}" "${raw:17:3}" "${raw:20:12}"
}

device_id=${SOAK_DEVICE_ID:-}
if [[ -n $device_id ]]; then
  device_id=$(printf '%s' "$device_id" | tr '[:upper:]' '[:lower:]')
elif [[ -f $device_id_file ]]; then
  IFS= read -r device_id <"$device_id_file"
else
  device_id=$(random_hex 32)
  device_tmp="${device_id_file}.tmp.$$"
  printf '%s\n' "$device_id" >"$device_tmp"
  chmod 600 "$device_tmp"
  mv "$device_tmp" "$device_id_file"
fi

if [[ ! $device_id =~ ^[a-f0-9]{64}$ ]]; then
  echo "device ID must contain exactly 64 hexadecimal characters: $device_id_file" >&2
  exit 65
fi

if [[ -f $identity_file ]]; then
  if ! jq -e \
    --arg account_id "$SOAK_ACCOUNT_ID" \
    --arg device_id "$device_id" \
    '.schema_version == 1
     and .account_id == $account_id
     and .device_id == $device_id
     and (.sessions | type == "object")' \
    "$identity_file" >/dev/null; then
    echo "existing session identity file does not match this account/device: $identity_file" >&2
    exit 65
  fi
else
  identity_tmp="${identity_file}.tmp.$$"
  jq -n \
    --arg account_id "$SOAK_ACCOUNT_ID" \
    --arg device_id "$device_id" \
    '{schema_version: 1, account_id: $account_id, device_id: $device_id, sessions: {}}' \
    >"$identity_tmp"
  chmod 600 "$identity_tmp"
  mv "$identity_tmp" "$identity_file"
fi

for session_name in "$@"; do
  if jq -e --arg session "$session_name" '.sessions[$session].session_id | type == "string"' "$identity_file" >/dev/null; then
    continue
  fi
  session_id=$(new_uuid)
  identity_tmp="${identity_file}.tmp.$$"
  jq \
    --arg session "$session_name" \
    --arg session_id "$session_id" \
    '.sessions[$session] = {session_id: $session_id}' \
    "$identity_file" >"$identity_tmp"
  chmod 600 "$identity_tmp"
  mv "$identity_tmp" "$identity_file"
done

if ! jq -e '
  (.device_id | test("^[a-f0-9]{64}$"))
  and ([.sessions[].session_id] | length > 0)
  and (all(.sessions[].session_id; test("^[a-f0-9]{8}-[a-f0-9]{4}-[1-5][a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$")))
  and (([.sessions[].session_id] | length) == ([.sessions[].session_id] | unique | length))
' "$identity_file" >/dev/null; then
  echo "session identity validation failed: $identity_file" >&2
  exit 65
fi

for session_name in "$@"; do
  if ! jq -e --arg session "$session_name" '.sessions[$session].session_id | type == "string"' "$identity_file" >/dev/null; then
    echo "session identity is missing after initialization: $session_name" >&2
    exit 65
  fi
done

session_count=$(jq '.sessions | length' "$identity_file")
printf 'session identities ready: file=%s device=%s... sessions=%s\n' \
  "$identity_file" "${device_id:0:12}" "$session_count"
