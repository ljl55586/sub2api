#!/usr/bin/env python3

import json
import os
import random
import subprocess
import sys
import tempfile
import threading
import unittest
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Dict, List
from unittest import mock


TEST_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(TEST_DIR))

import load_test  # noqa: E402


class FakeResponsesHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    records: List[Dict[str, Any]] = []
    counts: Dict[str, int] = {}
    lock = threading.Lock()
    force_status = 200

    @classmethod
    def reset(cls, force_status: int = 200) -> None:
        with cls.lock:
            cls.records = []
            cls.counts = {}
            cls.force_status = force_status

    def log_message(self, _format: str, *_args: Any) -> None:
        return

    def do_POST(self) -> None:
        content_length = int(self.headers.get("Content-Length", "0"))
        raw_body = self.rfile.read(content_length)
        body = json.loads(raw_body)
        session_id = self.headers.get("X-Sub2API-Codex-Session-Id", "")
        with self.lock:
            count = self.counts.get(session_id, 0) + 1
            self.counts[session_id] = count
            self.records.append(
                {
                    "session_id": session_id,
                    "profile": self.headers.get("X-Sub2API-Codex-Profile"),
                    "authorization_ok": self.headers.get("Authorization") == "Bearer integration-secret-key",
                    "body": body,
                }
            )

        if self.force_status != 200:
            payload = json.dumps({"error": {"message": "synthetic overload"}}).encode("utf-8")
            self.send_response(self.force_status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return

        answer = f"这是会话 {session_id} 的第 {count} 轮回答。"
        delta = json.dumps(
            {"type": "response.output_text.delta", "delta": answer},
            ensure_ascii=False,
            separators=(",", ":"),
        )
        completed = json.dumps(
            {
                "type": "response.completed",
                "response": {
                    "id": f"resp_{count}",
                    "status": "completed",
                    "output": [
                        {
                            "type": "message",
                            "role": "assistant",
                            "content": [{"type": "output_text", "text": answer}],
                        }
                    ],
                    "usage": {
                        "input_tokens": 400,
                        "output_tokens": 20,
                        "input_tokens_details": {
                            "cached_tokens": 0 if count == 1 else 256,
                            "cache_write_tokens": 256 if count == 1 else 0,
                        },
                    },
                },
            },
            ensure_ascii=False,
            separators=(",", ":"),
        )
        payload = (
            f"event: response.output_text.delta\ndata: {delta}\n\n"
            f"event: response.completed\ndata: {completed}\n\n"
        ).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("X-Sub2API-Codex-Session-Id", session_id)
        self.end_headers()
        self.wfile.write(payload)


class FakeServer:
    def __init__(self, force_status: int = 200) -> None:
        FakeResponsesHandler.reset(force_status)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), FakeResponsesHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self) -> "FakeServer":
        self.thread.start()
        return self

    def __exit__(self, _exc_type: Any, _exc: Any, _traceback: Any) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    @property
    def url(self) -> str:
        host, port = self.server.server_address
        return f"http://{host}:{port}/v1/responses"


def write_scenario(path: Path, sessions: int = 2, questions: int = 3) -> None:
    payload = {
        "name": "integration-database-indexes",
        "instructions": "使用中文回答，不要使用工具。",
        "topic_context": "只讨论数据库索引。",
        "questions": [f"问题 {index}" for index in range(1, questions + 1)],
        "sessions": sessions,
    }
    path.write_text(json.dumps(payload, ensure_ascii=False), encoding="utf-8")


class LoadTestUnitTests(unittest.TestCase):
    def test_request_history_is_append_only(self) -> None:
        scenario = load_test.Scenario(
            name="indexes",
            instructions="不要使用工具。",
            topic_context="数据库索引背景。",
            questions=("第一题", "第二题"),
            sessions=1,
            source_path="test",
        )
        session_id = str(uuid.uuid4())
        history = load_test.initial_history(scenario)
        first = load_test.build_request_body(scenario, session_id, history)
        assistant = load_test.message("assistant", "output_text", "第一题回答")
        load_test.append_completed_turn(history, assistant, scenario.questions[1])
        second = load_test.build_request_body(scenario, session_id, history)

        self.assertEqual(first["input"], second["input"][: len(first["input"])])
        self.assertEqual(session_id, first["prompt_cache_key"])
        self.assertEqual(session_id, second["prompt_cache_key"])
        self.assertNotIn("tools", first)
        self.assertNotIn("tool_choice", first)
        self.assertNotIn("parallel_tool_calls", first)
        self.assertNotIn("max_output_tokens", first)
        self.assertIn(load_test.NO_TOOL_POLICY, first["instructions"])
        self.assertTrue(first["instructions"].endswith(scenario.instructions))

        explicitly_capped = load_test.build_request_body(
            scenario,
            session_id,
            history,
            max_output_tokens=4096,
        )
        self.assertEqual(4096, explicitly_capped["max_output_tokens"])

    def test_uuid7_has_expected_version_and_variant(self) -> None:
        first = uuid.UUID(load_test.uuid7())
        second = uuid.UUID(load_test.uuid7())
        self.assertEqual(7, first.version)
        self.assertEqual(uuid.RFC_4122, first.variant)
        self.assertNotEqual(first, second)

    def test_random_delay_is_bounded_and_reproducible(self) -> None:
        first_rng = random.Random(42)
        second_rng = random.Random(42)
        first = [load_test.choose_delay(first_rng, 60, 300) for _ in range(100)]
        second = [load_test.choose_delay(second_rng, 60, 300) for _ in range(100)]
        self.assertEqual(first, second)
        self.assertTrue(all(60 <= value <= 300 for value in first))

    def test_parse_completed_sse_and_usage(self) -> None:
        terminal = {
            "type": "response.completed",
            "response": {
                "status": "completed",
                "output": [
                    {
                        "type": "message",
                        "role": "assistant",
                        "content": [{"type": "output_text", "text": "完整回答"}],
                    }
                ],
                "usage": {
                    "input_tokens": 120,
                    "output_tokens": 20,
                    "input_tokens_details": {"cached_tokens": 80, "cache_write_tokens": 10},
                },
            },
        }
        sse = (
            'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"增量"}\n\n'
            f"event: response.completed\ndata: {json.dumps(terminal, ensure_ascii=False)}\n\n"
        )
        result = load_test.parse_sse(sse)
        self.assertTrue(result.success)
        self.assertEqual("完整回答", result.assistant_message["content"][0]["text"])
        self.assertEqual(80, result.usage["cached_tokens"])
        self.assertEqual(10, result.usage["cache_write_tokens"])

    def test_parse_failed_truncated_and_tool_sse(self) -> None:
        failed = 'event: response.failed\ndata: {"type":"response.failed","response":{"error":{"message":"bad"}}}\n\n'
        failed_result = load_test.parse_sse(failed)
        self.assertFalse(failed_result.success)
        self.assertEqual("bad", failed_result.error)

        truncated_result = load_test.parse_sse(
            'data: {"type":"response.output_text.delta","delta":"partial"}\n\n'
        )
        self.assertFalse(truncated_result.success)
        self.assertIn("before response.completed", truncated_result.error)

        incomplete = {
            "type": "response.incomplete",
            "response": {
                "status": "incomplete",
                "incomplete_details": {"reason": "max_output_tokens"},
                "output": [
                    {
                        "type": "message",
                        "role": "assistant",
                        "content": [{"type": "output_text", "text": "不能续接的半截回答"}],
                    }
                ],
            },
        }
        incomplete_sse = (
            'data: {"type":"response.output_text.delta","delta":"不能续接的半截回答"}\n\n'
            f"data: {json.dumps(incomplete, ensure_ascii=False)}\n\n"
        )
        incomplete_result = load_test.parse_sse(incomplete_sse)
        self.assertFalse(incomplete_result.success)
        self.assertIsNone(incomplete_result.assistant_message)
        self.assertEqual("max_output_tokens", incomplete_result.error)

        tool = {
            "type": "response.completed",
            "response": {
                "status": "completed",
                "output": [{"type": "function_call", "name": "unexpected"}],
                "usage": {},
            },
        }
        tool_result = load_test.parse_sse(f"data: {json.dumps(tool)}\n\n")
        self.assertFalse(tool_result.success)
        self.assertTrue(tool_result.saw_tool_call)

    def test_completed_sse_without_usage_is_success_with_unknown_cache(self) -> None:
        terminal = {
            "type": "response.completed",
            "response": {
                "status": "completed",
                "output": [
                    {
                        "type": "message",
                        "role": "assistant",
                        "content": [{"type": "output_text", "text": "回答"}],
                    }
                ],
            },
        }
        result = load_test.parse_sse(f"data: {json.dumps(terminal, ensure_ascii=False)}\n\n")
        self.assertTrue(result.success)
        self.assertIsNone(result.usage["cached_tokens"])

    def test_curl_argv_and_environment_do_not_contain_api_key(self) -> None:
        secret = "secret-that-must-not-leak"
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            request_path = root / "request.json"
            response_path = root / "response.sse"
            headers_path = root / "headers.txt"
            request_path.write_text("{}", encoding="utf-8")
            command = load_test.build_curl_command(
                "http://127.0.0.1/v1/responses", request_path, response_path, headers_path
            )
            self.assertNotIn(secret, " ".join(command))
            config = load_test.build_curl_stdin_config(secret, str(uuid.uuid4()))
            self.assertIn(secret, config)

            fake_completed = subprocess.CompletedProcess(command, 0, stdout='{"http_code":200}', stderr="")
            with mock.patch.dict(os.environ, {load_test.API_KEY_ENV: secret}, clear=False):
                with mock.patch("load_test.subprocess.run", return_value=fake_completed) as run_mock:
                    load_test.execute_curl(
                        "http://127.0.0.1/v1/responses",
                        secret,
                        str(uuid.uuid4()),
                        request_path,
                        response_path,
                        headers_path,
                    )
            called_command = run_mock.call_args.args[0]
            called_environment = run_mock.call_args.kwargs["env"]
            self.assertNotIn(secret, " ".join(called_command))
            self.assertNotIn(load_test.API_KEY_ENV, called_environment)

    def test_cache_misses_warn_without_failing_session_summary(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            run_dir = Path(temp_dir)
            session_dir = run_dir / "sessions" / "session-1"
            events_path = session_dir / "events.jsonl"
            for turn in (1, 2, 3):
                load_test.append_jsonl(
                    events_path,
                    {
                        "event": "turn",
                        "turn": turn,
                        "status": "completed",
                        "http_status": 200,
                        "curl_metrics": {"time_total": 1.0, "time_starttransfer": 0.5},
                        "usage": {
                            "input_tokens": 100,
                            "output_tokens": 10,
                            "cached_tokens": 0,
                            "cache_write_tokens": 100,
                        },
                    },
                )
            workers = [
                {
                    "session_dir": str(session_dir),
                    "session_id": "session-1",
                    "scenario": "test",
                    "session_index": 1,
                    "turns": 3,
                    "returncode": 0,
                }
            ]
            summary = load_test.build_summary(run_dir, workers, False)
            self.assertEqual(1, summary["sessions"]["successful"])
            self.assertEqual(0.0, summary["cache"]["turn_hit_rate"])
            self.assertTrue(summary["warnings"])


class LoadTestIntegrationTests(unittest.TestCase):
    def run_controller(
        self,
        server_url: str,
        scenario_path: Path,
        output_dir: Path,
        sessions: int,
        turns: int,
        skip_preflight: bool,
    ) -> subprocess.CompletedProcess[str]:
        command = [
            sys.executable,
            str(TEST_DIR / "load_test.py"),
            server_url,
            "--scenario",
            str(scenario_path),
            "--sessions",
            str(sessions),
            "--turns",
            str(turns),
            "--min-delay",
            "0",
            "--max-delay",
            "0",
            "--seed",
            "1234",
            "--output-dir",
            str(output_dir),
        ]
        if skip_preflight:
            command.append("--skip-preflight")
        environment = os.environ.copy()
        environment[load_test.API_KEY_ENV] = "integration-secret-key"
        return subprocess.run(
            command,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=environment,
            timeout=30,
            check=False,
        )

    def test_two_sessions_three_turns_with_real_curl(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, FakeServer() as fake:
            root = Path(temp_dir)
            scenario_path = root / "scenario.json"
            output_dir = root / "run"
            write_scenario(scenario_path, sessions=2, questions=3)
            completed = self.run_controller(fake.url, scenario_path, output_dir, 2, 3, False)

            self.assertEqual(0, completed.returncode, msg=completed.stdout + "\n" + completed.stderr)
            self.assertNotIn("integration-secret-key", completed.stdout)
            self.assertNotIn("integration-secret-key", completed.stderr)
            summary = json.loads((output_dir / "summary.json").read_text(encoding="utf-8"))
            self.assertEqual(2, summary["sessions"]["successful"])
            self.assertEqual(6, summary["requests"]["successful"])
            self.assertEqual(1.0, summary["cache"]["turn_hit_rate"])

            workers = json.loads((output_dir / "workers.json").read_text(encoding="utf-8"))
            load_sessions = {worker["session_id"] for worker in workers}
            self.assertEqual(2, len(load_sessions))
            self.assertTrue(all(uuid.UUID(value).version == 7 for value in load_sessions))
            with FakeResponsesHandler.lock:
                records = list(FakeResponsesHandler.records)
            self.assertEqual(7, len(records), "one preflight plus six load requests")

            for session_id in load_sessions:
                session_records = [record for record in records if record["session_id"] == session_id]
                self.assertEqual(3, len(session_records))
                self.assertTrue(all(record["profile"] == "text" for record in session_records))
                self.assertTrue(all(record["authorization_ok"] for record in session_records))
                for index, record in enumerate(session_records):
                    body = record["body"]
                    self.assertEqual(session_id, body["prompt_cache_key"])
                    self.assertNotIn("tools", body)
                    self.assertNotIn("tool_choice", body)
                    self.assertNotIn("max_output_tokens", body)
                    self.assertIn(load_test.NO_TOOL_POLICY, body["instructions"])
                    self.assertEqual(2 + index * 2, len(body["input"]))
                    if index:
                        previous = session_records[index - 1]["body"]["input"]
                        self.assertEqual(previous, body["input"][: len(previous)])

            for path in output_dir.rglob("*"):
                if path.is_file():
                    self.assertNotIn("integration-secret-key", path.read_text(encoding="utf-8", errors="replace"))

    def test_429_is_not_retried(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, FakeServer(force_status=429) as fake:
            root = Path(temp_dir)
            scenario_path = root / "scenario.json"
            output_dir = root / "run"
            write_scenario(scenario_path, sessions=1, questions=1)
            completed = self.run_controller(fake.url, scenario_path, output_dir, 1, 1, True)

            self.assertEqual(1, completed.returncode, msg=completed.stdout + "\n" + completed.stderr)
            with FakeResponsesHandler.lock:
                records = list(FakeResponsesHandler.records)
            self.assertEqual(1, len(records))
            summary = json.loads((output_dir / "summary.json").read_text(encoding="utf-8"))
            self.assertEqual(1, summary["requests"]["failed"])
            self.assertEqual({"429": 1}, summary["requests"]["http_statuses"])


if __name__ == "__main__":
    unittest.main()
