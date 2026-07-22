#!/usr/bin/env bash
set -euo pipefail
umask 077

session_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
export SOAK_SCRIPT_DIR=${SOAK_SCRIPT_DIR:-$(cd -- "$session_dir/.." && pwd)}
source "$SOAK_SCRIPT_DIR/lib/runtime.sh"
soak_init_runtime

if [[ $# -ne 4 ]]; then
  echo "usage: $0 <first-turn> <cold|ttl_miss> <gap-min-seconds> <gap-max-seconds>" >&2
  exit 64
fi

soak_send_burst \
  session-c \
  "$SOAK_SCRIPT_DIR/prompts/session-c-redis-delay-queue.md" \
  "$1" "$2" "$3" "$4"
