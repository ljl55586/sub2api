# sub2api Codex curl load test

This tool runs multiple independent, append-only Responses conversations. Each
request is sent by the real `curl` binary while Python manages conversation
state, SSE parsing, random pacing, and reports.

## Run

The sub2api gateway must enable `gateway.curl_codex_profile`, and the supplied
API key must route to an OpenAI OAuth passthrough account.

```bash
./tools/codex-loadtest/run.sh \
  https://sub2api.example.com/v1/responses \
  sk-your-sub2api-key
```

The default scenario starts four sessions, asks ten database-index questions
per session, and waits a uniformly random 60 to 300 seconds between successful
turns. The first request is a separate preflight and is not included in load
statistics.

Every generated session ID and `prompt_cache_key` is UUIDv7, matching the
shape produced by current Codex CLI sessions. The load test uses the `text`
profile, so sub2api does not advertise tools upstream. It also prepends a
stable instruction that tells the model to answer directly without browsing,
reading files, running commands, simulating tool use, or delegating agents.
Scenario authors should still choose questions answerable from stable general
knowledge; wording alone is not a substitute for the tool-free request shape.

Useful overrides:

```bash
./tools/codex-loadtest/run.sh URL KEY \
  --sessions 2 \
  --turns 3 \
  --min-delay 60 \
  --max-delay 300 \
  --seed 42
```

By default the request omits `max_output_tokens`, matching Codex CLI 0.145.0.
This avoids a small client-side cap cutting off either reasoning or visible
answer tokens and making the turn unusable as the next request's prefix. To
deliberately impose a cap for a separate experiment, pass
`--max-output-tokens N`. An incomplete response always stops that session and
is never appended to its conversation history. GPT-5.6 Sol's published
128,000-token maximum output is a model capability ceiling, not a value that
Codex CLI sends in each request; see the official [model
page](https://developers.openai.com/api/docs/models/gpt-5.6-sol).

Generate requests without accessing the API:

```bash
./tools/codex-loadtest/run.sh \
  https://sub2api.example.com/v1/responses placeholder \
  --dry-run --min-delay 1 --max-delay 2
```

Pass `--scenario path/to/scenario.json` more than once to run different topics
in parallel. `--sessions` overrides the `sessions` value in every selected
scenario, and `--turns` limits the number of questions used.

The API key is passed from `run.sh` to Python through
`SUB2API_LOADTEST_API_KEY`, then to curl through curl's standard-input config.
It is not included in curl's command-line arguments or written to result files.

## Results

Runs are written under `loadtest-results/<run-id>/` beside this README unless
`--output-dir` is supplied. Each session has its own event log and per-turn
request, raw SSE response, response headers, and curl metrics. Aggregate
`summary.json` and `summary.md` report latency, HTTP status, token usage, and
post-warmup cache hit rates.

Cache misses do not fail the run. A post-warmup turn hit rate below 80 percent
is reported as a warning. HTTP failures, malformed or truncated SSE, unexpected
tool calls, and failed worker processes return a non-zero exit status.

## Test

Tests use a local fake SSE server and never contact a real upstream account.

```bash
python3 -m unittest discover -s tools/codex-loadtest -p 'test_*.py'
```

Prompt caching depends on exact prompt prefixes and a stable
`prompt_cache_key`; see the official [Prompt Caching
guide](https://developers.openai.com/api/docs/guides/prompt-caching).
