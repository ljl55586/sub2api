#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <anthropic-sse-file> <output-json-file>" >&2
  exit 64
fi

input_file=$1
output_file=$2

if [[ ! -f $input_file ]]; then
  echo "SSE input not found: $input_file" >&2
  exit 66
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required to reconstruct an Anthropic SSE response" >&2
  exit 69
fi

tmp_events=$(mktemp "${TMPDIR:-/tmp}/sub2api-anthropic-sse.XXXXXX")
trap 'rm -f "$tmp_events"' EXIT

awk '
  /^data:/ {
    line = $0
    sub(/^data:[[:space:]]?/, "", line)
    if (line != "" && line != "[DONE]") print line
  }
' "$input_file" >"$tmp_events"

if ! jq -s -e '
  length > 0
  and (
    any(.[]; .type == "error")
    or (
      any(.[]; .type == "message_start")
      and any(.[]; .type == "message_stop")
    )
  )
' "$tmp_events" >/dev/null; then
  echo "incomplete or invalid Anthropic SSE response: $input_file" >&2
  exit 65
fi

jq -s '
  reduce .[] as $event (
    {
      response: {
        id: "",
        type: "message",
        role: "assistant",
        model: "",
        content: [],
        stop_reason: null,
        stop_sequence: null,
        usage: {}
      },
      tool_input_json: {},
      stream_error: null
    };
    if $event.type == "message_start" then
      .response = ($event.message // .response)
      | .response.content = []
      | .response.usage = ($event.message.usage // {})
    elif $event.type == "content_block_start" then
      .response.content[$event.index] = $event.content_block
      | if $event.content_block.type == "tool_use" then
          .tool_input_json[($event.index | tostring)] = ""
        else
          .
        end
    elif $event.type == "content_block_delta" and $event.delta.type == "text_delta" then
      .response.content[$event.index].text =
        ((.response.content[$event.index].text // "") + ($event.delta.text // ""))
    elif $event.type == "content_block_delta" and $event.delta.type == "thinking_delta" then
      .response.content[$event.index].thinking =
        ((.response.content[$event.index].thinking // "") + ($event.delta.thinking // ""))
    elif $event.type == "content_block_delta" and $event.delta.type == "signature_delta" then
      .response.content[$event.index].signature =
        ((.response.content[$event.index].signature // "") + ($event.delta.signature // ""))
    elif $event.type == "content_block_delta" and $event.delta.type == "input_json_delta" then
      .tool_input_json[($event.index | tostring)] =
        ((.tool_input_json[($event.index | tostring)] // "") + ($event.delta.partial_json // ""))
    elif $event.type == "message_delta" then
      .response.stop_reason = ($event.delta.stop_reason // .response.stop_reason)
      | .response.stop_sequence = ($event.delta.stop_sequence // .response.stop_sequence)
      | .response.usage = ((.response.usage // {}) * ($event.usage // {}))
    elif $event.type == "error" then
      .stream_error = {
        type: "error",
        error: ($event.error // {type: "api_error", message: "unknown streaming error"}),
        request_id: ($event.request_id // null)
      }
    else
      .
    end
  )
  | if .stream_error != null then
      .stream_error
    else
      .response as $response
      | .tool_input_json as $tool_input_json
      | $response
      | .content = [
          range(0; ($response.content | length)) as $index
          | $response.content[$index]
          | select(. != null)
          | if .type == "tool_use" then
              .input = (
                $tool_input_json[($index | tostring)] as $raw
                | if ($raw // "") == "" then
                    (.input // {})
                  else
                    try ($raw | fromjson)
                    catch error("invalid tool_use input JSON at content index \($index)")
                  end
              )
            else
              .
            end
        ]
    end
' "$tmp_events" >"$output_file"

