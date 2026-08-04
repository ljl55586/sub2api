#!/bin/sh
set -eu

if [ "$#" -lt 2 ]; then
  echo "Usage: $0 API_URL API_KEY [load_test.py options...]" >&2
  exit 2
fi

api_url=$1
export SUB2API_LOADTEST_API_KEY=$2
shift 2

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec python3 "$script_dir/load_test.py" "$api_url" "$@"
