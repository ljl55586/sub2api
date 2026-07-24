#!/usr/bin/env bash
set -euo pipefail

output_file=""
header_file=""
request_file=""
approval_id=""
approval_token=""
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
    --header)
      case "$2" in
        X-Sub2API-Upstream-Approval-ID:\ *)
          approval_id=${2#X-Sub2API-Upstream-Approval-ID: }
          ;;
        X-Sub2API-Upstream-Approval-Token:\ *)
          approval_token=${2#X-Sub2API-Upstream-Approval-Token: }
          ;;
      esac
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

if [[ -n ${FAKE_CURL_UPSTREAM_APPROVAL_DIR:-} ]]; then
  : "${approval_id:?fake curl did not receive the upstream approval ID header}"
  if [[ -n ${FAKE_CURL_EXPECT_APPROVAL_TOKEN:-} &&
    $approval_token != "$FAKE_CURL_EXPECT_APPROVAL_TOKEN" ]]; then
    echo "fake curl received an incorrect upstream approval token" >&2
    exit 64
  fi
  stage_id="${approval_id}-s01"
  preview_file="$FAKE_CURL_UPSTREAM_APPROVAL_DIR/$stage_id.preview.json"
  preview_tmp="$FAKE_CURL_UPSTREAM_APPROVAL_DIR/.$stage_id.preview.tmp"
  user_turns=$(jq '[.messages[] | select(.role == "user")] | length' "$request_file")
  if (( user_turns == 1 )); then
    request_kinds='["quota","title","main"]'
  else
    request_kinds='["main"]'
  fi
  jq -n \
    --arg approval_id "$approval_id" \
    --arg stage_id "$stage_id" \
    --argjson request_kinds "$request_kinds" \
    --slurpfile body "$request_file" '
      {
        version: 1,
        approval_id: $approval_id,
        stage_id: $stage_id,
        created_at: "2026-07-24T00:00:00Z",
        network_sent: false,
        requests: [
          $request_kinds[] as $kind
          | {
              kind: $kind,
              method: "POST",
              url: "https://api.anthropic.com/v1/messages?beta=true",
              headers: [
                {name: "authorization", values: ["Bearer [redacted]"]},
                {name: "anthropic-version", values: ["2023-06-01"]}
              ],
              body_sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
              content_length: ($body[0] | tostring | length),
              body: $body[0]
            }
        ]
      }
    ' >"$preview_tmp"
  chmod 600 "$preview_tmp"
  mv -f -- "$preview_tmp" "$preview_file"

  while [[ ! -f "$FAKE_CURL_UPSTREAM_APPROVAL_DIR/$stage_id.approve" &&
    ! -f "$FAKE_CURL_UPSTREAM_APPROVAL_DIR/$stage_id.reject" ]]; do
    sleep 0.02
  done
  if [[ -f "$FAKE_CURL_UPSTREAM_APPROVAL_DIR/$stage_id.reject" ]]; then
    printf '%s\n' \
      '{"type":"error","error":{"type":"upstream_approval_rejected","message":"rejected by test runner"}}' \
      >"$output_file"
    printf 'HTTP/1.1 409 Fake\r\nContent-Type: application/json\r\n\r\n' >"$header_file"
    printf '409\t0.010'
    exit 0
  fi
fi

cp -- "$response_file" "$output_file"
printf 'HTTP/1.1 %s Fake\r\nContent-Type: %s\r\n\r\n' \
  "$FAKE_CURL_HTTP_CODE" "$FAKE_CURL_CONTENT_TYPE" >"$header_file"
printf '%s\t0.010' "$FAKE_CURL_HTTP_CODE"
