#!/usr/bin/env python3
"""Multi-session curl load test for the sub2api Codex text profile.

The controller starts one child process per conversation. Each worker keeps an
append-only Responses API input history and invokes the real curl binary for
every turn. Only Python's standard library is required.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import math
import os
import random
import secrets
import signal
import subprocess
import sys
import time
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Sequence, Tuple
from urllib.parse import urlparse


SCRIPT_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parents[1]
BACKEND_DIR = REPO_ROOT / "backend"
DEFAULT_SCENARIO = SCRIPT_DIR / "scenarios" / "database-indexes.json"
API_KEY_ENV = "SUB2API_LOADTEST_API_KEY"
REVIEW_REQUEST_ENV = "SUB2API_CODEX_LOADTEST_REVIEW_REQUEST"
REVIEW_OUTPUT_ENV = "SUB2API_CODEX_LOADTEST_REVIEW_OUTPUT"
REVIEW_SESSION_ENV = "SUB2API_CODEX_LOADTEST_REVIEW_SESSION_ID"
REVIEW_GO_TEST = "^TestBuildCurlCodexLoadtestUpstreamPreview$"
CACHE_WARNING_THRESHOLD = 0.80
DEFAULT_MODEL = "gpt-5.6-sol"
DEFAULT_REASONING_EFFORT = "high"
DEFAULT_MIN_DELAY_SECONDS = 60
DEFAULT_MAX_DELAY_SECONDS = 300
DEFAULT_CONNECT_TIMEOUT_SECONDS = 15
DEFAULT_REQUEST_TIMEOUT_SECONDS = 900
NO_TOOL_POLICY = (
    "本压测请求只包含可以依据已提供上下文和稳定通用知识直接回答的问题。"
    "不要调用、请求、模拟、建议或声称使用任何工具；不要联网、搜索、读取文件、执行命令，"
    "也不要委派其他代理。信息略有不足时，说明最小合理假设后直接回答，不要用工具验证。"
)


class LoadTestError(RuntimeError):
    """Expected configuration, protocol, or request failure."""


@dataclass(frozen=True)
class Scenario:
    name: str
    instructions: str
    topic_context: str
    questions: Tuple[str, ...]
    sessions: int
    source_path: str


@dataclass
class SSEParseResult:
    success: bool
    terminal_type: Optional[str]
    assistant_message: Optional[Dict[str, Any]]
    usage: Dict[str, Optional[int]]
    error: Optional[str]
    event_count: int
    saw_tool_call: bool


@dataclass
class CurlResult:
    returncode: int
    metrics: Dict[str, Any]
    response_headers: Dict[str, str]
    stderr: str
    response_text: str


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def uuid7() -> str:
    """Return a standards-shaped UUIDv7 using only the Python standard library."""
    unix_ms = int(time.time() * 1000) & ((1 << 48) - 1)
    random_a = secrets.randbits(12)
    random_b = secrets.randbits(62)
    value = (
        (unix_ms << 80)
        | (0x7 << 76)
        | (random_a << 64)
        | (0x2 << 62)
        | random_b
    )
    return str(uuid.UUID(int=value))


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def append_jsonl(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(value, ensure_ascii=False, separators=(",", ":")) + "\n")


def read_jsonl(path: Path) -> List[Dict[str, Any]]:
    if not path.exists():
        return []
    rows: List[Dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.strip():
            rows.append(json.loads(line))
    return rows


def require_nonempty_string(raw: Dict[str, Any], key: str, path: Path) -> str:
    value = raw.get(key)
    if not isinstance(value, str) or not value.strip():
        raise LoadTestError(f"{path}: {key} must be a non-empty string")
    return value.strip()


def load_scenario(path: Path) -> Scenario:
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise LoadTestError(f"cannot load scenario {path}: {exc}") from exc
    if not isinstance(raw, dict):
        raise LoadTestError(f"{path}: scenario must be a JSON object")

    name = require_nonempty_string(raw, "name", path)
    instructions = require_nonempty_string(raw, "instructions", path)
    topic_context = require_nonempty_string(raw, "topic_context", path)
    questions_raw = raw.get("questions")
    if not isinstance(questions_raw, list) or not questions_raw:
        raise LoadTestError(f"{path}: questions must be a non-empty array")
    questions: List[str] = []
    for index, value in enumerate(questions_raw, start=1):
        if not isinstance(value, str) or not value.strip():
            raise LoadTestError(f"{path}: questions[{index}] must be a non-empty string")
        questions.append(value.strip())
    sessions = raw.get("sessions")
    if not isinstance(sessions, int) or isinstance(sessions, bool) or sessions < 1:
        raise LoadTestError(f"{path}: sessions must be a positive integer")
    return Scenario(
        name=name,
        instructions=instructions,
        topic_context=topic_context,
        questions=tuple(questions),
        sessions=sessions,
        source_path=str(path.resolve()),
    )


def validate_api_url(api_url: str) -> str:
    value = api_url.strip()
    parsed = urlparse(value)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        raise LoadTestError("API URL must be an absolute http(s) URL")
    if parsed.query or parsed.fragment:
        raise LoadTestError("API URL must not contain a query string or fragment")
    if not parsed.path.rstrip("/").endswith("/responses"):
        raise LoadTestError("API URL must point to a Responses endpoint ending in /responses")
    return value


def message(role: str, content_type: str, text: str) -> Dict[str, Any]:
    return {
        "type": "message",
        "role": role,
        "content": [{"type": content_type, "text": text}],
    }


def initial_history(scenario: Scenario) -> List[Dict[str, Any]]:
    return [
        message("user", "input_text", scenario.topic_context),
        message("user", "input_text", scenario.questions[0]),
    ]


def build_request_body(
    scenario: Scenario,
    session_id: str,
    history: Sequence[Dict[str, Any]],
    max_output_tokens: Optional[int] = None,
) -> Dict[str, Any]:
    if max_output_tokens is not None and max_output_tokens < 1:
        raise LoadTestError("max_output_tokens must be a positive integer when set")
    body = {
        "model": DEFAULT_MODEL,
        "instructions": f"{NO_TOOL_POLICY}\n\n{scenario.instructions}",
        "input": copy.deepcopy(list(history)),
        "reasoning": {"effort": DEFAULT_REASONING_EFFORT},
        "text": {"verbosity": "low"},
        "prompt_cache_key": session_id,
        "store": False,
        "stream": True,
    }
    # Codex CLI 0.145.0 does not serialize max_output_tokens on its main
    # Responses request. Omitting it lets the selected model apply its own
    # output ceiling and avoids turning a completed conversational turn into
    # an incomplete response merely because of a small client-side cap.
    if max_output_tokens is not None:
        body["max_output_tokens"] = max_output_tokens
    return body


def append_completed_turn(
    history: List[Dict[str, Any]],
    assistant_message: Dict[str, Any],
    next_question: Optional[str],
) -> None:
    previous = copy.deepcopy(history)
    history.append(copy.deepcopy(assistant_message))
    if next_question is not None:
        history.append(message("user", "input_text", next_question))
    if history[: len(previous)] != previous:
        raise LoadTestError("conversation invariant failed: a prior input item was modified")


def json_digest(value: Any) -> str:
    encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def build_upstream_preview(
    request_path: Path,
    session_id: str,
    turn_dir: Path,
) -> Tuple[Dict[str, Any], Path]:
    """Run the production Go request builder in-process without networking."""
    if not (BACKEND_DIR / "go.mod").is_file():
        raise LoadTestError(f"cannot locate backend Go module at {BACKEND_DIR}")
    preview_path = turn_dir / "upstream-preview.json"
    build_log_path = turn_dir / "upstream-preview-build.log"
    environment = os.environ.copy()
    # The preview uses a synthetic account and never needs the real API key.
    # Do not leak the key into the Go test process or its diagnostic output.
    environment.pop(API_KEY_ENV, None)
    environment[REVIEW_REQUEST_ENV] = str(request_path.resolve())
    environment[REVIEW_OUTPUT_ENV] = str(preview_path.resolve())
    environment[REVIEW_SESSION_ENV] = session_id
    command = [
        "go",
        "test",
        "./internal/service",
        "-run",
        REVIEW_GO_TEST,
        "-count=1",
    ]
    try:
        completed = subprocess.run(
            command,
            cwd=str(BACKEND_DIR),
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=environment,
            timeout=180,
            check=False,
        )
    except FileNotFoundError as exc:
        raise LoadTestError("review mode requires the Go toolchain in PATH") from exc
    except subprocess.TimeoutExpired as exc:
        raise LoadTestError("upstream preview builder timed out after 180 seconds") from exc

    build_log = completed.stdout + completed.stderr
    build_log_path.write_text(build_log, encoding="utf-8")
    if completed.returncode != 0:
        tail = build_log[-2000:].strip()
        raise LoadTestError(f"upstream preview builder failed (rc={completed.returncode}): {tail}")
    if not preview_path.is_file():
        raise LoadTestError("upstream preview builder succeeded without producing its output file")
    try:
        preview = json.loads(preview_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise LoadTestError(f"cannot read upstream preview: {exc}") from exc
    if not isinstance(preview, dict) or not isinstance(preview.get("body"), dict):
        raise LoadTestError("upstream preview is missing a JSON request body")
    write_json(turn_dir / "upstream-body.json", preview["body"])
    return preview, preview_path


def build_prefix_report(first_preview: Dict[str, Any], second_preview: Dict[str, Any]) -> Dict[str, Any]:
    first_body = first_preview.get("body")
    second_body = second_preview.get("body")
    if not isinstance(first_body, dict) or not isinstance(second_body, dict):
        raise LoadTestError("upstream preview body must be an object")
    first_input = first_body.get("input")
    second_input = second_body.get("input")
    if not isinstance(first_input, list) or not isinstance(second_input, list):
        raise LoadTestError("upstream preview input must be an array")
    prefix_slice = second_input[: len(first_input)]
    stable_fields = {
        key: first_body.get(key) == second_body.get(key)
        for key in (
            "model",
            "reasoning",
            "text",
            "store",
            "stream",
            "prompt_cache_key",
            "max_output_tokens",
        )
    }
    exact_prefix = len(second_input) >= len(first_input) and prefix_slice == first_input
    return {
        "prompt_cache_key_equal": stable_fields["prompt_cache_key"],
        "first_input_is_exact_prefix": exact_prefix,
        "all_cache_invariants_pass": exact_prefix and all(stable_fields.values()),
        "first_input_items": len(first_input),
        "second_input_items": len(second_input),
        "new_input_items": copy.deepcopy(second_input[len(first_input) :]),
        "first_input_sha256": json_digest(first_input),
        "second_prefix_sha256": json_digest(prefix_slice),
        "stable_fields_equal": stable_fields,
    }


def review_and_confirm(
    turn_number: int,
    preview: Dict[str, Any],
    preview_path: Path,
    prefix_report: Optional[Dict[str, Any]],
    prefix_report_path: Optional[Path],
) -> bool:
    body = preview["body"]
    print("", flush=True)
    print("=" * 72, flush=True)
    print(f"REVIEW REQUIRED: turn {turn_number} has NOT been sent", flush=True)
    print(f"Method/URL: {preview.get('method')} {preview.get('url')}", flush=True)
    print(f"Body bytes: {preview.get('body_bytes')}  SHA-256: {preview.get('body_sha256')}", flush=True)
    print(f"Model: {body.get('model')}", flush=True)
    print(f"Prompt cache key: {body.get('prompt_cache_key')}", flush=True)
    print(f"Input items: {len(body.get('input', []))}", flush=True)
    print(f"Exact redacted upstream preview: {preview_path}", flush=True)
    print(f"Upstream body only: {preview_path.parent / 'upstream-body.json'}", flush=True)
    if prefix_report is not None:
        print(f"Prefix report: {prefix_report_path}", flush=True)
        print(
            "First upstream input is exact second-turn prefix: "
            f"{prefix_report['first_input_is_exact_prefix']}",
            flush=True,
        )
        print(
            f"Cache invariants all pass: {prefix_report['all_cache_invariants_pass']}",
            flush=True,
        )
        print(
            f"Input items: {prefix_report['first_input_items']} -> {prefix_report['second_input_items']}",
            flush=True,
        )
    print("Inspect the files above now. Authorization is redacted; the request body is complete.", flush=True)
    try:
        answer = input(f"Type YES to send turn {turn_number}; anything else stops: ")
    except EOFError:
        return False
    return answer.strip() == "YES"


def choose_delay(rng: random.Random, minimum: int, maximum: int) -> int:
    if minimum < 0 or maximum < minimum:
        raise LoadTestError("delay range must satisfy 0 <= min-delay <= max-delay")
    return rng.randint(minimum, maximum)


def iter_sse_events(text: str) -> Iterable[Tuple[Optional[str], str]]:
    event_name: Optional[str] = None
    data_lines: List[str] = []
    normalized = text.replace("\r\n", "\n").replace("\r", "\n")
    for line in normalized.split("\n"):
        if line == "":
            if data_lines:
                yield event_name, "\n".join(data_lines)
            event_name = None
            data_lines = []
            continue
        if line.startswith(":"):
            continue
        field, separator, value = line.partition(":")
        if separator and value.startswith(" "):
            value = value[1:]
        if field == "event":
            event_name = value
        elif field == "data":
            data_lines.append(value)
    if data_lines:
        yield event_name, "\n".join(data_lines)


def integer_at(value: Any, *paths: Sequence[str]) -> Optional[int]:
    for path in paths:
        current = value
        for key in path:
            if not isinstance(current, dict) or key not in current:
                current = None
                break
            current = current[key]
        if isinstance(current, int) and not isinstance(current, bool):
            return current
    return None


def extract_usage(response: Dict[str, Any]) -> Dict[str, Optional[int]]:
    usage = response.get("usage")
    if not isinstance(usage, dict):
        return {
            "input_tokens": None,
            "output_tokens": None,
            "cached_tokens": None,
            "cache_write_tokens": None,
        }
    return {
        "input_tokens": integer_at(usage, ("input_tokens",), ("prompt_tokens",)),
        "output_tokens": integer_at(usage, ("output_tokens",), ("completion_tokens",)),
        "cached_tokens": integer_at(
            usage,
            ("input_tokens_details", "cached_tokens"),
            ("prompt_tokens_details", "cached_tokens"),
            ("cache_read_input_tokens",),
        ),
        "cache_write_tokens": integer_at(
            usage,
            ("input_tokens_details", "cache_write_tokens"),
            ("input_tokens_details", "cache_creation_tokens"),
            ("prompt_tokens_details", "cache_write_tokens"),
            ("prompt_tokens_details", "cache_creation_tokens"),
            ("cache_creation_input_tokens",),
        ),
    }


def is_tool_item(item: Any) -> bool:
    if not isinstance(item, dict):
        return False
    item_type = str(item.get("type") or "").strip().lower()
    if not item_type or item_type in {"message", "reasoning"}:
        return False
    return (
        item_type.endswith("_call")
        or item_type.endswith("_call_output")
        or item_type in {"function_call", "custom_tool_call", "tool_call"}
    )


def assistant_from_response(response: Dict[str, Any], delta_text: str) -> Optional[Dict[str, Any]]:
    output = response.get("output")
    if isinstance(output, list):
        for item in output:
            if not isinstance(item, dict) or item.get("type") != "message" or item.get("role") != "assistant":
                continue
            content = item.get("content")
            if not isinstance(content, list):
                continue
            cleaned: List[Dict[str, Any]] = []
            for block in content:
                if not isinstance(block, dict):
                    continue
                block_type = block.get("type")
                block_text = block.get("text")
                if block_type == "output_text" and isinstance(block_text, str):
                    cleaned.append({"type": "output_text", "text": block_text})
            if cleaned:
                return {"type": "message", "role": "assistant", "content": cleaned}
    if delta_text:
        return message("assistant", "output_text", delta_text)
    return None


def response_error_message(payload: Dict[str, Any], response: Dict[str, Any]) -> str:
    for candidate in (payload.get("error"), response.get("error"), response.get("incomplete_details")):
        if isinstance(candidate, dict):
            message_value = candidate.get("message") or candidate.get("reason") or candidate.get("code")
            if message_value:
                return str(message_value)
        elif candidate:
            return str(candidate)
    return "upstream returned a terminal failure event"


def parse_sse(text: str) -> SSEParseResult:
    delta_parts: List[str] = []
    terminal_type: Optional[str] = None
    terminal_response: Optional[Dict[str, Any]] = None
    terminal_error: Optional[str] = None
    event_count = 0
    saw_tool_call = False
    invalid_events = 0

    for event_name, data in iter_sse_events(text):
        if data.strip() == "[DONE]":
            continue
        try:
            payload = json.loads(data)
        except json.JSONDecodeError:
            invalid_events += 1
            continue
        if not isinstance(payload, dict):
            invalid_events += 1
            continue
        event_count += 1
        event_type = str(payload.get("type") or event_name or "")
        if event_type == "response.output_text.delta" and isinstance(payload.get("delta"), str):
            delta_parts.append(payload["delta"])

        for possible_item in (payload.get("item"), payload.get("output_item")):
            if is_tool_item(possible_item):
                saw_tool_call = True

        if event_type in {"response.completed", "response.done"}:
            response = payload.get("response")
            if isinstance(response, dict):
                terminal_type = event_type
                terminal_response = response
        elif event_type in {
            "response.failed",
            "response.incomplete",
            "response.cancelled",
            "response.canceled",
        }:
            response = payload.get("response")
            response_object = response if isinstance(response, dict) else {}
            terminal_type = event_type
            terminal_response = response_object
            terminal_error = response_error_message(payload, response_object)

    empty_usage = {
        "input_tokens": None,
        "output_tokens": None,
        "cached_tokens": None,
        "cache_write_tokens": None,
    }
    if terminal_response is None:
        detail = "stream ended before response.completed"
        if invalid_events:
            detail += f" ({invalid_events} invalid SSE data frame(s))"
        return SSEParseResult(False, terminal_type, None, empty_usage, detail, event_count, saw_tool_call)

    output = terminal_response.get("output")
    if isinstance(output, list) and any(is_tool_item(item) for item in output):
        saw_tool_call = True
    if saw_tool_call:
        return SSEParseResult(
            False,
            terminal_type,
            None,
            extract_usage(terminal_response),
            "text profile received an unexpected tool call",
            event_count,
            True,
        )
    if terminal_error:
        return SSEParseResult(
            False,
            terminal_type,
            None,
            extract_usage(terminal_response),
            terminal_error,
            event_count,
            False,
        )
    response_status = str(terminal_response.get("status") or "completed").lower()
    if response_status in {"failed", "incomplete", "cancelled", "canceled"}:
        return SSEParseResult(
            False,
            terminal_type,
            None,
            extract_usage(terminal_response),
            response_error_message({}, terminal_response),
            event_count,
            False,
        )

    assistant = assistant_from_response(terminal_response, "".join(delta_parts))
    if assistant is None:
        return SSEParseResult(
            False,
            terminal_type,
            None,
            extract_usage(terminal_response),
            "completed response did not contain assistant text",
            event_count,
            False,
        )
    return SSEParseResult(
        True,
        terminal_type,
        assistant,
        extract_usage(terminal_response),
        None,
        event_count,
        False,
    )


def parse_response_headers(text: str) -> Dict[str, str]:
    blocks: List[List[str]] = []
    current: List[str] = []
    for line in text.replace("\r\n", "\n").replace("\r", "\n").split("\n"):
        if line == "":
            if current:
                blocks.append(current)
                current = []
            continue
        current.append(line)
    if current:
        blocks.append(current)
    http_blocks = [block for block in blocks if block and block[0].upper().startswith("HTTP/")]
    selected = http_blocks[-1] if http_blocks else (blocks[-1] if blocks else [])
    headers: Dict[str, str] = {}
    for line in selected[1:] if selected and selected[0].upper().startswith("HTTP/") else selected:
        name, separator, value = line.partition(":")
        if separator:
            headers[name.strip().lower()] = value.strip()
    return headers


def curl_config_quote(value: str) -> str:
    if "\n" in value or "\r" in value:
        raise LoadTestError("header values must not contain newlines")
    return value.replace("\\", "\\\\").replace('"', '\\"')


def build_curl_command(
    api_url: str,
    request_path: Path,
    response_path: Path,
    headers_path: Path,
) -> List[str]:
    return [
        "curl",
        "--config",
        "-",
        "--request",
        "POST",
        "--data-binary",
        "@" + str(request_path),
        "--dump-header",
        str(headers_path),
        "--output",
        str(response_path),
        "--write-out",
        "%{json}",
        api_url,
    ]


def build_curl_stdin_config(api_key: str, session_id: str) -> str:
    headers = [
        "Content-Type: application/json",
        "Accept: text/event-stream",
        f"Authorization: Bearer {api_key}",
        "X-Sub2API-Codex-Profile: text",
        f"X-Sub2API-Codex-Session-Id: {session_id}",
    ]
    lines = [
        "silent",
        "show-error",
        "no-buffer",
        "fail-with-body",
        f"connect-timeout = {DEFAULT_CONNECT_TIMEOUT_SECONDS}",
        f"max-time = {DEFAULT_REQUEST_TIMEOUT_SECONDS}",
    ]
    lines.extend(f'header = "{curl_config_quote(header)}"' for header in headers)
    return "\n".join(lines) + "\n"


def parse_curl_metrics(stdout: str) -> Dict[str, Any]:
    value = stdout.strip()
    if not value:
        return {}
    try:
        decoded = json.loads(value)
    except json.JSONDecodeError:
        return {"write_out_parse_error": value[:500]}
    return decoded if isinstance(decoded, dict) else {}


def execute_curl(
    api_url: str,
    api_key: str,
    session_id: str,
    request_path: Path,
    response_path: Path,
    headers_path: Path,
) -> CurlResult:
    command = build_curl_command(api_url, request_path, response_path, headers_path)
    config = build_curl_stdin_config(api_key, session_id)
    curl_env = os.environ.copy()
    # The worker receives the key through its environment, but curl only
    # needs the stdin config. Do not expose the key in curl's environment.
    curl_env.pop(API_KEY_ENV, None)
    try:
        completed = subprocess.run(
            command,
            input=config,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=DEFAULT_REQUEST_TIMEOUT_SECONDS + 30,
            check=False,
            env=curl_env,
        )
        stderr = completed.stderr.replace(api_key, "[REDACTED]")
        returncode = completed.returncode
        metrics = parse_curl_metrics(completed.stdout)
    except OSError as exc:
        stderr = f"cannot execute curl: {exc}"
        returncode = 127
        metrics = {"curl_start_error": str(exc)}
    except subprocess.TimeoutExpired as exc:
        stderr_value = exc.stderr.decode() if isinstance(exc.stderr, bytes) else (exc.stderr or "")
        stderr = (stderr_value + "\ncurl subprocess timeout").replace(api_key, "[REDACTED]")
        returncode = 124
        metrics = {"timeout": True}

    response_text = response_path.read_text(encoding="utf-8", errors="replace") if response_path.exists() else ""
    headers_text = headers_path.read_text(encoding="utf-8", errors="replace") if headers_path.exists() else ""
    return CurlResult(
        returncode=returncode,
        metrics=metrics,
        response_headers=parse_response_headers(headers_text),
        stderr=stderr.strip(),
        response_text=response_text,
    )


def metric_http_code(metrics: Dict[str, Any]) -> Optional[int]:
    value = metrics.get("http_code")
    if isinstance(value, int) and not isinstance(value, bool):
        return value
    if isinstance(value, str) and value.isdigit():
        return int(value)
    return None


def execute_turn(
    api_url: str,
    api_key: str,
    session_id: str,
    body: Dict[str, Any],
    turn_dir: Path,
    dry_run: bool,
    turn_number: int,
) -> Tuple[Dict[str, Any], Optional[Dict[str, Any]], Dict[str, str]]:
    turn_dir.mkdir(parents=True, exist_ok=True)
    request_path = turn_dir / "request.json"
    response_path = turn_dir / "response.sse"
    headers_path = turn_dir / "headers.txt"
    metrics_path = turn_dir / "curl-metrics.json"
    write_json(request_path, body)

    started_at = utc_now()
    monotonic_start = time.monotonic()
    if dry_run:
        response_path.write_text("", encoding="utf-8")
        headers_path.write_text("", encoding="utf-8")
        metrics = {"dry_run": True, "http_code": 0, "time_total": 0.0, "time_starttransfer": 0.0}
        write_json(metrics_path, metrics)
        assistant = message("assistant", "output_text", f"DRY-RUN 第 {turn_number} 轮占位回答。")
        event = {
            "status": "dry_run",
            "started_at": started_at,
            "finished_at": utc_now(),
            "elapsed_seconds": 0.0,
            "curl_returncode": 0,
            "http_status": None,
            "curl_metrics": metrics,
            "usage": {
                "input_tokens": None,
                "output_tokens": None,
                "cached_tokens": None,
                "cache_write_tokens": None,
            },
            "sse_terminal_type": None,
            "sse_event_count": 0,
            "error": None,
            "paths": {
                "request": str(request_path),
                "response": str(response_path),
                "headers": str(headers_path),
                "curl_metrics": str(metrics_path),
            },
        }
        return event, assistant, {}

    curl_result = execute_curl(
        api_url,
        api_key,
        session_id,
        request_path,
        response_path,
        headers_path,
    )
    write_json(metrics_path, curl_result.metrics)
    http_status = metric_http_code(curl_result.metrics)
    parsed = parse_sse(curl_result.response_text)
    error: Optional[str] = None
    if curl_result.returncode != 0:
        error = f"curl exited with code {curl_result.returncode}"
        if curl_result.stderr:
            error += f": {curl_result.stderr[:1000]}"
    elif http_status is None or not 200 <= http_status < 300:
        error = f"unexpected HTTP status {http_status}"
    elif not parsed.success:
        error = parsed.error or "invalid SSE response"

    event = {
        "status": "completed" if error is None else "failed",
        "started_at": started_at,
        "finished_at": utc_now(),
        "elapsed_seconds": round(time.monotonic() - monotonic_start, 6),
        "curl_returncode": curl_result.returncode,
        "http_status": http_status,
        "curl_metrics": curl_result.metrics,
        "usage": parsed.usage,
        "sse_terminal_type": parsed.terminal_type,
        "sse_event_count": parsed.event_count,
        "saw_tool_call": parsed.saw_tool_call,
        "error": error,
        "paths": {
            "request": str(request_path),
            "response": str(response_path),
            "headers": str(headers_path),
            "curl_metrics": str(metrics_path),
        },
    }
    return event, parsed.assistant_message if error is None else None, curl_result.response_headers


def worker_seed(base_seed: int, scenario_name: str, session_index: int) -> int:
    digest = hashlib.sha256(f"{base_seed}:{scenario_name}:{session_index}".encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "big")


def run_worker(args: argparse.Namespace) -> int:
    scenario_paths = args.scenario or [str(DEFAULT_SCENARIO)]
    if len(scenario_paths) != 1:
        raise LoadTestError("worker requires exactly one --scenario")
    scenario = load_scenario(Path(scenario_paths[0]))
    turns = args.turns if args.turns is not None else len(scenario.questions)
    if turns < 1 or turns > len(scenario.questions):
        raise LoadTestError(f"turns must be between 1 and {len(scenario.questions)} for {scenario.name}")
    if args.review_two_turns and turns != 2:
        raise LoadTestError("review worker requires exactly two turns")
    if not args.session_id or not args.session_dir or args.session_index is None or args.worker_seed is None:
        raise LoadTestError("worker arguments are incomplete")
    try:
        uuid.UUID(args.session_id)
    except ValueError as exc:
        raise LoadTestError("worker session ID must be a UUID") from exc

    api_key = os.environ.get(API_KEY_ENV, "")
    if not args.dry_run and not api_key:
        raise LoadTestError(f"{API_KEY_ENV} is required")
    session_dir = Path(args.session_dir)
    session_dir.mkdir(parents=True, exist_ok=True)
    events_path = session_dir / "events.jsonl"
    rng = random.Random(args.worker_seed)
    history = initial_history(scenario)
    completed_turns = 0
    failed = False
    previous_upstream_preview: Optional[Dict[str, Any]] = None

    write_json(
        session_dir / "session.json",
        {
            "session_id": args.session_id,
            "session_index": args.session_index,
            "scenario": scenario.name,
            "scenario_path": scenario.source_path,
            "turns": turns,
            "seed": args.worker_seed,
            "dry_run": args.dry_run,
            "review_two_turns": args.review_two_turns,
            "started_at": utc_now(),
        },
    )
    print(f"[{scenario.name} session-{args.session_index}] started {args.session_id}", flush=True)

    for question_index in range(turns):
        turn_number = question_index + 1
        turn_dir = session_dir / f"turn-{turn_number:03d}"
        body = build_request_body(scenario, args.session_id, history, args.max_output_tokens)
        if "tools" in body or "tool_choice" in body or "parallel_tool_calls" in body:
            raise LoadTestError("text profile request must not advertise tools")
        preview: Optional[Dict[str, Any]] = None
        preview_path: Optional[Path] = None
        prefix_report: Optional[Dict[str, Any]] = None
        prefix_report_path: Optional[Path] = None
        if args.review_two_turns:
            turn_dir.mkdir(parents=True, exist_ok=True)
            request_path = turn_dir / "request.json"
            write_json(request_path, body)
            preview, preview_path = build_upstream_preview(
                request_path,
                args.session_id,
                turn_dir,
            )
            if previous_upstream_preview is not None:
                prefix_report = build_prefix_report(previous_upstream_preview, preview)
                prefix_report_path = turn_dir / "prefix-report.json"
                write_json(prefix_report_path, prefix_report)
                if not prefix_report["all_cache_invariants_pass"]:
                    write_json(
                        turn_dir / "review-decision.json",
                        {
                            "approved": False,
                            "reason": "cache prefix invariant failed",
                            "decided_at": utc_now(),
                            "preview_body_sha256": preview.get("body_sha256"),
                        },
                    )
                    raise LoadTestError(
                        f"turn {turn_number} upstream cache prefix invariant failed; request was not sent"
                    )
            approved = review_and_confirm(
                turn_number,
                preview,
                preview_path,
                prefix_report,
                prefix_report_path,
            )
            write_json(
                turn_dir / "review-decision.json",
                {
                    "approved": approved,
                    "reason": "user approved" if approved else "user declined or input closed",
                    "decided_at": utc_now(),
                    "preview_body_sha256": preview.get("body_sha256"),
                },
            )
            if not approved:
                failed = True
                event = {
                    "event": "turn",
                    "status": "failed",
                    "sent": False,
                    "started_at": utc_now(),
                    "finished_at": utc_now(),
                    "elapsed_seconds": 0.0,
                    "curl_returncode": None,
                    "http_status": None,
                    "curl_metrics": {},
                    "usage": {
                        "input_tokens": None,
                        "output_tokens": None,
                        "cached_tokens": None,
                        "cache_write_tokens": None,
                    },
                    "sse_terminal_type": None,
                    "sse_event_count": 0,
                    "error": "review declined before send",
                    "session_id": args.session_id,
                    "session_index": args.session_index,
                    "scenario": scenario.name,
                    "turn": turn_number,
                    "question": scenario.questions[question_index],
                    "planned_wait_seconds": None,
                    "cache_hit": None,
                    "paths": {
                        "request": str(request_path),
                        "upstream_preview": str(preview_path),
                        "prefix_report": str(prefix_report_path) if prefix_report_path else None,
                    },
                }
                append_jsonl(events_path, event)
                print(
                    f"[{scenario.name} session-{args.session_index}] turn {turn_number} was not sent",
                    file=sys.stderr,
                    flush=True,
                )
                break
        event, assistant, response_headers = execute_turn(
            args.api_url,
            api_key,
            args.session_id,
            body,
            turn_dir,
            args.dry_run,
            turn_number,
        )
        event["sent"] = not args.dry_run
        if args.review_two_turns and event["status"] == "completed":
            returned_session = response_headers.get("x-sub2api-codex-session-id")
            if returned_session != args.session_id:
                event["status"] = "failed"
                event["error"] = (
                    "review request did not receive the expected X-Sub2API-Codex-Session-Id; "
                    "verify the deployed gateway has curl_codex_profile enabled"
                )
                assistant = None
        if preview_path is not None:
            event["paths"]["upstream_preview"] = str(preview_path)
            event["paths"]["upstream_body"] = str(turn_dir / "upstream-body.json")
        if prefix_report_path is not None:
            event["paths"]["prefix_report"] = str(prefix_report_path)
        wait_seconds: Optional[int] = None
        if event["status"] in {"completed", "dry_run"} and turn_number < turns:
            wait_seconds = 0 if args.review_two_turns else choose_delay(rng, args.min_delay, args.max_delay)
        event.update(
            {
                "event": "turn",
                "session_id": args.session_id,
                "session_index": args.session_index,
                "scenario": scenario.name,
                "turn": turn_number,
                "question": scenario.questions[question_index],
                "planned_wait_seconds": wait_seconds,
                "cache_hit": (
                    event["usage"].get("cached_tokens") > 0
                    if turn_number > 1 and isinstance(event["usage"].get("cached_tokens"), int)
                    else None
                ),
            }
        )
        append_jsonl(events_path, event)

        if event["status"] not in {"completed", "dry_run"} or assistant is None:
            failed = True
            print(
                f"[{scenario.name} session-{args.session_index}] turn {turn_number} failed: {event['error']}",
                file=sys.stderr,
                flush=True,
            )
            break
        completed_turns += 1
        cached_tokens = event["usage"].get("cached_tokens")
        print(
            f"[{scenario.name} session-{args.session_index}] turn {turn_number}/{turns} "
            f"{event['status']} cached_tokens={cached_tokens}",
            flush=True,
        )
        next_question = scenario.questions[question_index + 1] if turn_number < turns else None
        append_completed_turn(history, assistant, next_question)
        if preview is not None:
            previous_upstream_preview = preview
        if wait_seconds is not None and not args.dry_run:
            time.sleep(wait_seconds)

    result = {
        "session_id": args.session_id,
        "session_index": args.session_index,
        "scenario": scenario.name,
        "expected_turns": turns,
        "completed_turns": completed_turns,
        "success": not failed and completed_turns == turns,
        "finished_at": utc_now(),
    }
    write_json(session_dir / "worker-result.json", result)
    return 0 if result["success"] else 1


def run_preflight(
    api_url: str,
    api_key: str,
    run_dir: Path,
    max_output_tokens: Optional[int],
) -> None:
    session_id = uuid7()
    scenario = Scenario(
        name="preflight",
        instructions=(
            "使用中文回答，只回复 OK。不要调用、请求或假装使用任何工具，也不要输出代码块。"
        ),
        topic_context="这是 sub2api Codex text profile 的连接预检。",
        questions=("请只回复 OK。",),
        sessions=1,
        source_path="<built-in>",
    )
    body = build_request_body(
        scenario,
        session_id,
        initial_history(scenario),
        max_output_tokens,
    )
    event, _, response_headers = execute_turn(
        api_url,
        api_key,
        session_id,
        body,
        run_dir / "preflight" / "turn-001",
        False,
        1,
    )
    returned_session = response_headers.get("x-sub2api-codex-session-id")
    if event["status"] != "completed":
        raise LoadTestError(f"preflight failed: {event['error']}")
    if returned_session != session_id:
        raise LoadTestError(
            "preflight did not receive the expected X-Sub2API-Codex-Session-Id; "
            "verify gateway.curl_codex_profile is enabled"
        )
    write_json(
        run_dir / "preflight" / "result.json",
        {
            "success": True,
            "session_id": session_id,
            "returned_session_id": returned_session,
            "completed_at": utc_now(),
        },
    )


def percentile(values: Sequence[float], fraction: float) -> Optional[float]:
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return round(float(ordered[0]), 6)
    position = (len(ordered) - 1) * fraction
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        value = ordered[lower]
    else:
        value = ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)
    return round(float(value), 6)


def numeric_metric(event: Dict[str, Any], key: str) -> Optional[float]:
    metrics = event.get("curl_metrics")
    if not isinstance(metrics, dict):
        return None
    value = metrics.get(key)
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return float(value)
    return None


def sum_usage(events: Sequence[Dict[str, Any]], key: str) -> int:
    total = 0
    for event in events:
        usage = event.get("usage")
        value = usage.get(key) if isinstance(usage, dict) else None
        if isinstance(value, int) and not isinstance(value, bool):
            total += value
    return total


def build_summary(
    run_dir: Path,
    workers: Sequence[Dict[str, Any]],
    dry_run: bool,
    cache_threshold: float = CACHE_WARNING_THRESHOLD,
) -> Dict[str, Any]:
    all_events: List[Dict[str, Any]] = []
    session_rows: List[Dict[str, Any]] = []
    for worker in workers:
        session_dir = Path(worker["session_dir"])
        events = [event for event in read_jsonl(session_dir / "events.jsonl") if event.get("event") == "turn"]
        all_events.extend(events)
        expected = int(worker["turns"])
        completed = sum(event.get("status") in {"completed", "dry_run"} for event in events)
        session_rows.append(
            {
                "session_id": worker["session_id"],
                "scenario": worker["scenario"],
                "session_index": worker["session_index"],
                "expected_turns": expected,
                "completed_turns": completed,
                "process_returncode": worker.get("returncode"),
                "success": worker.get("returncode") == 0 and completed == expected,
            }
        )

    # A declined review is a session failure but not an attempted HTTP request.
    # Dry-run turns still count as generated requests in dry-run summaries.
    request_events = [
        event
        for event in all_events
        if event.get("status") == "dry_run" or event.get("sent") is not False
    ]
    completed_events = [event for event in request_events if event.get("status") in {"completed", "dry_run"}]
    failed_events = [event for event in request_events if event.get("status") == "failed"]
    http_statuses: Dict[str, int] = {}
    for event in request_events:
        status = event.get("http_status")
        if status is not None:
            key = str(status)
            http_statuses[key] = http_statuses.get(key, 0) + 1

    total_latencies = [value for event in completed_events if (value := numeric_metric(event, "time_total")) is not None]
    ttfb_latencies = [
        value for event in completed_events if (value := numeric_metric(event, "time_starttransfer")) is not None
    ]

    eligible = [
        event
        for event in completed_events
        if isinstance(event.get("turn"), int) and int(event["turn"]) > 1 and not dry_run
    ]
    known_cache_events: List[Dict[str, Any]] = []
    for event in eligible:
        usage = event.get("usage")
        if isinstance(usage, dict) and isinstance(usage.get("cached_tokens"), int):
            known_cache_events.append(event)
    cache_hits = sum(event["usage"]["cached_tokens"] > 0 for event in known_cache_events)
    cache_hit_rate = cache_hits / len(known_cache_events) if known_cache_events else None
    post_warm_input = sum_usage(eligible, "input_tokens")
    post_warm_cached = sum_usage(eligible, "cached_tokens")
    token_cache_ratio = post_warm_cached / post_warm_input if post_warm_input else None

    warnings: List[str] = []
    if not dry_run:
        if not known_cache_events:
            warnings.append("No post-warmup response exposed cached_tokens; cache hit rate is unknown.")
        elif cache_hit_rate is not None and cache_hit_rate < cache_threshold:
            warnings.append(
                f"Post-warmup cache hit rate {cache_hit_rate:.1%} is below the {cache_threshold:.0%} warning threshold."
            )

    attempted = len(request_events)
    successful = len(completed_events)
    summary = {
        "run_dir": str(run_dir),
        "dry_run": dry_run,
        "generated_at": utc_now(),
        "sessions": {
            "total": len(session_rows),
            "successful": sum(row["success"] for row in session_rows),
            "failed": sum(not row["success"] for row in session_rows),
            "details": session_rows,
        },
        "requests": {
            "attempted": attempted,
            "successful": successful,
            "failed": len(failed_events),
            "success_rate": successful / attempted if attempted else None,
            "http_statuses": http_statuses,
        },
        "latency_seconds": {
            "total": {
                "p50": percentile(total_latencies, 0.50),
                "p95": percentile(total_latencies, 0.95),
                "p99": percentile(total_latencies, 0.99),
            },
            "ttfb": {
                "p50": percentile(ttfb_latencies, 0.50),
                "p95": percentile(ttfb_latencies, 0.95),
                "p99": percentile(ttfb_latencies, 0.99),
            },
        },
        "tokens": {
            "input": sum_usage(completed_events, "input_tokens"),
            "output": sum_usage(completed_events, "output_tokens"),
            "cached": sum_usage(completed_events, "cached_tokens"),
            "cache_write": sum_usage(completed_events, "cache_write_tokens"),
        },
        "cache": {
            "warmup_turns_excluded": True,
            "eligible_turns": len(eligible),
            "turns_with_cache_metrics": len(known_cache_events),
            "hit_turns": cache_hits,
            "turn_hit_rate": cache_hit_rate,
            "token_cache_ratio": token_cache_ratio,
            "warning_threshold": cache_threshold,
        },
        "warnings": warnings,
    }
    return summary


def format_value(value: Any, percent: bool = False) -> str:
    if value is None:
        return "n/a"
    if percent:
        return f"{float(value):.1%}"
    if isinstance(value, float):
        return f"{value:.3f}"
    return str(value)


def write_summary_files(run_dir: Path, summary: Dict[str, Any]) -> None:
    write_json(run_dir / "summary.json", summary)
    requests = summary["requests"]
    sessions = summary["sessions"]
    latency = summary["latency_seconds"]
    cache = summary["cache"]
    tokens = summary["tokens"]
    lines = [
        "# sub2api Codex Load Test Summary",
        "",
        f"- Generated: {summary['generated_at']}",
        f"- Dry run: {summary['dry_run']}",
        f"- Sessions: {sessions['successful']}/{sessions['total']} successful",
        f"- Requests: {requests['successful']}/{requests['attempted']} successful",
        f"- Request success rate: {format_value(requests['success_rate'], percent=True)}",
        f"- Cache turn hit rate after warmup: {format_value(cache['turn_hit_rate'], percent=True)}",
        f"- Cache token ratio after warmup: {format_value(cache['token_cache_ratio'], percent=True)}",
        "",
        "## Latency",
        "",
        "| Metric | p50 | p95 | p99 |",
        "| --- | ---: | ---: | ---: |",
        f"| Total seconds | {format_value(latency['total']['p50'])} | {format_value(latency['total']['p95'])} | {format_value(latency['total']['p99'])} |",
        f"| TTFB seconds | {format_value(latency['ttfb']['p50'])} | {format_value(latency['ttfb']['p95'])} | {format_value(latency['ttfb']['p99'])} |",
        "",
        "## Tokens",
        "",
        f"- Input: {tokens['input']}",
        f"- Output: {tokens['output']}",
        f"- Cached: {tokens['cached']}",
        f"- Cache write: {tokens['cache_write']}",
        "",
        "## Sessions",
        "",
        "| Scenario | Session | Turns | Result |",
        "| --- | --- | ---: | --- |",
    ]
    for row in sessions["details"]:
        result = "success" if row["success"] else f"failed (rc={row['process_returncode']})"
        lines.append(
            f"| {row['scenario']} | {row['session_id']} | {row['completed_turns']}/{row['expected_turns']} | {result} |"
        )
    if summary["warnings"]:
        lines.extend(["", "## Warnings", ""])
        lines.extend(f"- {warning}" for warning in summary["warnings"])
    (run_dir / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")


def create_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Multi-session curl load test for sub2api Codex text profile")
    parser.add_argument("api_url", help="full sub2api Responses endpoint, for example https://host/v1/responses")
    parser.add_argument("--scenario", action="append", help="scenario JSON path; repeat to run multiple scenarios")
    parser.add_argument("--sessions", type=int, help="override sessions for every selected scenario")
    parser.add_argument("--turns", type=int, help="limit turns per session")
    parser.add_argument("--min-delay", type=int, default=DEFAULT_MIN_DELAY_SECONDS)
    parser.add_argument("--max-delay", type=int, default=DEFAULT_MAX_DELAY_SECONDS)
    parser.add_argument("--seed", type=int, help="base seed for reproducible per-session delays")
    parser.add_argument(
        "--max-output-tokens",
        type=int,
        help=(
            "optional explicit response cap; omitted by default to match Codex CLI 0.145.0 "
            "and avoid truncating a turn"
        ),
    )
    parser.add_argument("--output-dir", help="exact output directory for this run")
    parser.add_argument("--dry-run", action="store_true", help="generate all request files without calling curl")
    parser.add_argument("--skip-preflight", action="store_true")
    parser.add_argument(
        "--review-two-turns",
        action="store_true",
        help=(
            "run exactly one two-turn session; before each send, build and save the final "
            "redacted upstream request with production Go code and require typing YES"
        ),
    )
    parser.add_argument("--worker", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--session-id", help=argparse.SUPPRESS)
    parser.add_argument("--session-index", type=int, help=argparse.SUPPRESS)
    parser.add_argument("--session-dir", help=argparse.SUPPRESS)
    parser.add_argument("--worker-seed", type=int, help=argparse.SUPPRESS)
    return parser


def run_id() -> str:
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + f"-{os.getpid()}"


def controller_worker_command(
    args: argparse.Namespace,
    scenario: Scenario,
    session_id: str,
    session_index: int,
    session_dir: Path,
    seed: int,
    turns: int,
) -> List[str]:
    command = [
        sys.executable,
        str(Path(__file__).resolve()),
        args.api_url,
        "--worker",
        "--scenario",
        scenario.source_path,
        "--session-id",
        session_id,
        "--session-index",
        str(session_index),
        "--session-dir",
        str(session_dir),
        "--worker-seed",
        str(seed),
        "--turns",
        str(turns),
        "--min-delay",
        str(args.min_delay),
        "--max-delay",
        str(args.max_delay),
    ]
    if args.dry_run:
        command.append("--dry-run")
    if args.review_two_turns:
        command.append("--review-two-turns")
    if args.max_output_tokens is not None:
        command.extend(["--max-output-tokens", str(args.max_output_tokens)])
    return command


def terminate_processes(processes: Sequence[subprocess.Popen[Any]]) -> None:
    for process in processes:
        if process.poll() is None:
            process.terminate()
    deadline = time.monotonic() + 5
    for process in processes:
        if process.poll() is not None:
            continue
        timeout = max(0.0, deadline - time.monotonic())
        try:
            process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            process.kill()


def run_controller(args: argparse.Namespace) -> int:
    args.api_url = validate_api_url(args.api_url)
    choose_delay(random.Random(0), args.min_delay, args.max_delay)
    if args.sessions is not None and args.sessions < 1:
        raise LoadTestError("--sessions must be positive")
    if args.max_output_tokens is not None and args.max_output_tokens < 1:
        raise LoadTestError("--max-output-tokens must be positive")
    if args.review_two_turns and args.dry_run:
        raise LoadTestError("--review-two-turns cannot be combined with --dry-run")
    if args.review_two_turns and args.sessions not in {None, 1}:
        raise LoadTestError("--review-two-turns always uses exactly one session")
    if args.review_two_turns and args.turns not in {None, 2}:
        raise LoadTestError("--review-two-turns always uses exactly two turns")
    api_key = os.environ.get(API_KEY_ENV, "")
    if not args.dry_run and not api_key:
        raise LoadTestError(
            f"{API_KEY_ENV} is not set; use run.sh URL API_KEY or export the variable before running load_test.py"
        )

    scenario_paths = [Path(path) for path in (args.scenario or [str(DEFAULT_SCENARIO)])]
    if args.review_two_turns and len(scenario_paths) != 1:
        raise LoadTestError("--review-two-turns requires exactly one scenario")
    scenarios = [load_scenario(path) for path in scenario_paths]
    scenario_runtime: List[Tuple[Scenario, int, int]] = []
    for scenario in scenarios:
        session_count = 1 if args.review_two_turns else (args.sessions if args.sessions is not None else scenario.sessions)
        turns = 2 if args.review_two_turns else (args.turns if args.turns is not None else len(scenario.questions))
        if turns < 1 or turns > len(scenario.questions):
            raise LoadTestError(f"--turns must be between 1 and {len(scenario.questions)} for {scenario.name}")
        scenario_runtime.append((scenario, session_count, turns))
    base_seed = args.seed if args.seed is not None else secrets.randbits(63)
    output_dir = Path(args.output_dir).resolve() if args.output_dir else (SCRIPT_DIR / "loadtest-results" / run_id())
    try:
        output_dir.mkdir(parents=True, exist_ok=False)
    except OSError as exc:
        raise LoadTestError(f"cannot create output directory {output_dir}: {exc}") from exc
    try:
        output_dir.chmod(0o700)
    except OSError:
        pass

    run_metadata = {
        "api_url": args.api_url,
        "started_at": utc_now(),
        "base_seed": base_seed,
        "dry_run": args.dry_run,
        "preflight_enabled": not args.skip_preflight and not args.dry_run and not args.review_two_turns,
        "review_two_turns": args.review_two_turns,
        "min_delay_seconds": args.min_delay,
        "max_delay_seconds": args.max_delay,
        "model": DEFAULT_MODEL,
        "reasoning_effort": DEFAULT_REASONING_EFFORT,
        "max_output_tokens": args.max_output_tokens,
        "max_output_tokens_mode": (
            "explicit" if args.max_output_tokens is not None else "omitted_like_codex_cli_0.145.0"
        ),
        "profile": "text",
        "scenarios": [
            {
                "name": scenario.name,
                "path": scenario.source_path,
                "sessions": session_count,
                "available_turns": len(scenario.questions),
                "selected_turns": turns,
            }
            for scenario, session_count, turns in scenario_runtime
        ],
    }
    write_json(output_dir / "run.json", run_metadata)

    if not args.skip_preflight and not args.dry_run and not args.review_two_turns:
        print("Running preflight request...", flush=True)
        run_preflight(args.api_url, api_key, output_dir, args.max_output_tokens)
        print("Preflight succeeded.", flush=True)

    workers: List[Dict[str, Any]] = []
    processes: List[subprocess.Popen[Any]] = []
    global_index = 0
    for scenario, session_count, turns in scenario_runtime:
        for local_index in range(1, session_count + 1):
            global_index += 1
            session_id = uuid7()
            session_dir = output_dir / "sessions" / session_id
            seed = worker_seed(base_seed, scenario.name, local_index)
            command = controller_worker_command(
                args,
                scenario,
                session_id,
                local_index,
                session_dir,
                seed,
                turns,
            )
            worker = {
                "scenario": scenario.name,
                "session_id": session_id,
                "session_index": local_index,
                "global_index": global_index,
                "session_dir": str(session_dir),
                "turns": turns,
                "seed": seed,
                "command_without_secret": command,
            }
            workers.append(worker)
            try:
                processes.append(subprocess.Popen(command, env=os.environ.copy()))
            except OSError as exc:
                terminate_processes(processes)
                raise LoadTestError(f"cannot start session worker: {exc}") from exc

    print(f"Started {len(processes)} session worker(s). Results: {output_dir}", flush=True)
    interrupted = False
    try:
        for process, worker in zip(processes, workers):
            worker["returncode"] = process.wait()
    except KeyboardInterrupt:
        interrupted = True
        print("Interrupted; terminating session workers...", file=sys.stderr, flush=True)
        terminate_processes(processes)
        for process, worker in zip(processes, workers):
            worker["returncode"] = process.poll()

    write_json(output_dir / "workers.json", workers)
    summary = build_summary(output_dir, workers, args.dry_run)
    if interrupted:
        summary["warnings"].append("Controller was interrupted before all sessions completed.")
    write_summary_files(output_dir, summary)
    print(f"Summary: {output_dir / 'summary.md'}", flush=True)
    for warning in summary["warnings"]:
        print(f"WARNING: {warning}", file=sys.stderr, flush=True)

    process_failure = interrupted or any(worker.get("returncode") != 0 for worker in workers)
    return 1 if process_failure else 0


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = create_parser()
    args = parser.parse_args(argv)
    try:
        args.api_url = validate_api_url(args.api_url)
        if args.worker:
            return run_worker(args)
        return run_controller(args)
    except LoadTestError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, lambda _signum, _frame: sys.exit(143))
    raise SystemExit(main())
