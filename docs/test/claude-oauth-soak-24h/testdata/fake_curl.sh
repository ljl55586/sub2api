#!/usr/bin/env bash
set -euo pipefail

output_file=""
header_file=""
request_file=""
if [[ -n ${FAKE_CURL_ARGS_FILE:-} ]]; then
  printf '%s\n' "$@" >"$FAKE_CURL_ARGS_FILE"
fi
while (( $# > 0 )); do
  case "$1" in
    --output)
      output_file=$2
      shift 2
      ;;
    --dump-header)
      header_file=$2
      shift 2
      ;;
    --write-out)
      shift 2
      ;;
    --data-binary)
      request_file=${2#@}
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

: "${FAKE_CURL_HTTP_CODE:=200}"
: "${FAKE_CURL_CONTENT_TYPE:=application/json}"
response_file=${FAKE_CURL_RESPONSE_FILE:-}
if [[ -n ${FAKE_CURL_SEQUENCE_DIR:-} ]]; then
  : "${FAKE_CURL_SEQUENCE_COUNTER_FILE:?set FAKE_CURL_SEQUENCE_COUNTER_FILE}"
  call_count=0
  if [[ -f $FAKE_CURL_SEQUENCE_COUNTER_FILE ]]; then
    IFS= read -r call_count <"$FAKE_CURL_SEQUENCE_COUNTER_FILE"
  fi
  call_count=$((call_count + 1))
  printf '%s\n' "$call_count" >"$FAKE_CURL_SEQUENCE_COUNTER_FILE"
  response_file="$FAKE_CURL_SEQUENCE_DIR/$call_count.json"
fi
: "${response_file:?set FAKE_CURL_RESPONSE_FILE or FAKE_CURL_SEQUENCE_DIR}"
if [[ -n ${FAKE_CURL_CALLED_FILE:-} ]]; then
  printf '%s\n' called >"$FAKE_CURL_CALLED_FILE"
fi
if [[ -n ${FAKE_CURL_CAPTURE_REQUEST_FILE:-} ]]; then
  cp -- "$request_file" "$FAKE_CURL_CAPTURE_REQUEST_FILE"
fi
cp -- "$response_file" "$output_file"
printf 'HTTP/1.1 %s Fake\r\nContent-Type: %s\r\n\r\n' \
  "$FAKE_CURL_HTTP_CODE" "$FAKE_CURL_CONTENT_TYPE" >"$header_file"
printf '%s\t0.010' "$FAKE_CURL_HTTP_CODE"
