#!/usr/bin/env python3
"""Lightweight Sub2API group pressure test for OpenAI-compatible endpoints.

The script intentionally uses only Python standard library modules so it can run
on a fresh machine without installing locust/k6/aiohttp.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request
from collections import Counter
from dataclasses import dataclass
from typing import Any


@dataclass
class Result:
    ok: bool
    status: int
    latency_ms: float
    error: str


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Pressure test one Sub2API API key bound to a group.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument("--base-url", default="http://localhost:8080", help="Sub2API base URL")
    parser.add_argument(
        "--api-key",
        default=os.getenv("SUB2API_API_KEY", ""),
        help="Sub2API API key, or set SUB2API_API_KEY",
    )
    parser.add_argument(
        "--endpoint",
        choices=["responses", "chat"],
        default="responses",
        help="Gateway API shape to call",
    )
    parser.add_argument("--model", default="gpt-5.5", help="Model name to send")
    parser.add_argument("--total", type=int, default=100, help="Total requests to send")
    parser.add_argument("--concurrency", type=int, default=10, help="Concurrent workers")
    parser.add_argument("--timeout", type=float, default=120.0, help="Per request timeout in seconds")
    parser.add_argument(
        "--qps",
        type=float,
        default=0.0,
        help="Approximate global request rate limit. 0 means no client-side throttle",
    )
    parser.add_argument(
        "--prompt",
        default="Reply with exactly one short sentence. Load test request",
        help="Prompt prefix",
    )
    parser.add_argument(
        "--session-mode",
        choices=["unique", "fixed", "none"],
        default="unique",
        help=(
            "unique adds a different prompt_cache_key per request; fixed reuses one "
            "prompt_cache_key to test sticky sessions; none omits it"
        ),
    )
    parser.add_argument(
        "--max-output-tokens",
        type=int,
        default=32,
        help="Small output cap to reduce upstream cost when supported",
    )
    parser.add_argument("--show-errors", type=int, default=8, help="Number of error samples to print")
    return parser.parse_args()


def build_payload(args: argparse.Namespace, request_id: int) -> dict[str, Any]:
    text = f"{args.prompt} #{request_id}"
    if args.endpoint == "chat":
        payload: dict[str, Any] = {
            "model": args.model,
            "messages": [{"role": "user", "content": text}],
            "stream": False,
        }
        # OpenAI OAuth/Codex path strips unsupported token caps, while API-key
        # upstreams may honor this. Keeping it small is useful for cost control.
        payload["max_tokens"] = args.max_output_tokens
    else:
        payload = {
            "model": args.model,
            "instructions": "You are a concise assistant.",
            "input": text,
            "stream": False,
            "max_output_tokens": args.max_output_tokens,
        }

    if args.session_mode == "unique":
        payload["prompt_cache_key"] = f"load-test-{request_id}"
    elif args.session_mode == "fixed":
        payload["prompt_cache_key"] = "load-test-fixed"
    return payload


def summarize_error(raw: bytes) -> str:
    if not raw:
        return ""
    text = raw.decode("utf-8", errors="replace").strip()
    try:
        obj = json.loads(text)
    except json.JSONDecodeError:
        return text[:300]
    err = obj.get("error")
    if isinstance(err, dict):
        message = err.get("message") or err.get("code") or err.get("type")
        if message:
            return str(message)[:300]
    return text[:300]


def post_once(args: argparse.Namespace, request_id: int, throttle: "Throttle") -> Result:
    throttle.wait()
    url_path = "/v1/chat/completions" if args.endpoint == "chat" else "/v1/responses"
    url = args.base_url.rstrip("/") + url_path
    body = json.dumps(build_payload(args, request_id), ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=body,
        method="POST",
        headers={
            "Authorization": f"Bearer {args.api_key}",
            "Content-Type": "application/json",
            "Accept": "application/json",
            "User-Agent": "sub2api-group-pressure/1.0",
        },
    )

    started = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=args.timeout) as resp:
            raw = resp.read()
            latency_ms = (time.perf_counter() - started) * 1000
            status = resp.getcode()
            if 200 <= status < 300:
                return Result(True, status, latency_ms, "")
            return Result(False, status, latency_ms, summarize_error(raw))
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        latency_ms = (time.perf_counter() - started) * 1000
        return Result(False, exc.code, latency_ms, summarize_error(raw))
    except Exception as exc:  # noqa: BLE001 - CLI should report all request failures.
        latency_ms = (time.perf_counter() - started) * 1000
        return Result(False, 0, latency_ms, repr(exc))


class Throttle:
    def __init__(self, qps: float) -> None:
        self.interval = 1.0 / qps if qps and qps > 0 else 0.0
        self.next_at = time.perf_counter()
        self.lock = threading.Lock()

    def wait(self) -> None:
        if self.interval <= 0:
            return
        with self.lock:
            now = time.perf_counter()
            sleep_for = max(0.0, self.next_at - now)
            self.next_at = max(now, self.next_at) + self.interval
        if sleep_for > 0:
            time.sleep(sleep_for)


def percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, int(round((pct / 100.0) * (len(ordered) - 1)))))
    return ordered[index]


def print_summary(results: list[Result], elapsed: float, show_errors: int) -> None:
    total = len(results)
    ok = sum(1 for r in results if r.ok)
    failed = total - ok
    latencies = [r.latency_ms for r in results]
    statuses = Counter(r.status for r in results)
    errors = Counter(r.error for r in results if r.error)

    print("\n=== Summary ===")
    print(f"total={total} ok={ok} failed={failed} success_rate={(ok / total * 100 if total else 0):.2f}%")
    print(f"elapsed={elapsed:.2f}s throughput={(total / elapsed if elapsed > 0 else 0):.2f} req/s")
    if latencies:
        print(
            "latency_ms "
            f"avg={statistics.mean(latencies):.1f} "
            f"p50={percentile(latencies, 50):.1f} "
            f"p90={percentile(latencies, 90):.1f} "
            f"p95={percentile(latencies, 95):.1f} "
            f"p99={percentile(latencies, 99):.1f} "
            f"max={max(latencies):.1f}"
        )
    print("status_codes=" + json.dumps(dict(sorted(statuses.items())), ensure_ascii=False))

    if errors:
        print("\n=== Error samples ===")
        for message, count in errors.most_common(show_errors):
            print(f"[{count}] {message}")


def main() -> int:
    args = parse_args()
    if not args.api_key:
        print("Missing API key. Pass --api-key or set SUB2API_API_KEY.", file=sys.stderr)
        return 2
    if args.total <= 0 or args.concurrency <= 0:
        print("--total and --concurrency must be positive.", file=sys.stderr)
        return 2

    print(
        "Starting pressure test: "
        f"endpoint={args.endpoint} model={args.model} total={args.total} "
        f"concurrency={args.concurrency} session_mode={args.session_mode}"
    )

    throttle = Throttle(args.qps)
    results: list[Result] = []
    started = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        futures = [executor.submit(post_once, args, i, throttle) for i in range(1, args.total + 1)]
        for idx, future in enumerate(concurrent.futures.as_completed(futures), start=1):
            result = future.result()
            results.append(result)
            if idx % max(1, args.total // 10) == 0 or idx == args.total:
                print(f"progress {idx}/{args.total} ok={sum(1 for r in results if r.ok)}")

    elapsed = time.perf_counter() - started
    print_summary(results, elapsed, args.show_errors)
    return 0 if all(r.ok for r in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
