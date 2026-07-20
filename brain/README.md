# Overmind (`brain/`)

Overmind is a standalone Go process that plans a goal into a DAG, then
dispatches and monitors agent sessions on the AO daemon over its loopback
HTTP API. CLI binary: `om`.

## Build

```sh
cd brain
go build ./...
go build -o om ./cmd/om
```

## Run

```sh
./om --help
./om plan "add dark mode" --project /path/to/repo [--edit]
./om approve <plan-id>
./om run <plan-id>
./om status [plan-id]
./om events <plan-id>
```

All commands are Phase 1 stubs until the planner/scheduler land.

## Config

Config is read from `~/.overmind/config.yaml` (see
[`config.example.yaml`](./config.example.yaml)), with `OVERMIND_*` env vars
taking precedence. Defaults:

| Field | Default | Env override |
|---|---|---|
| `ao_base_url` | `http://127.0.0.1:3001` | `OVERMIND_AO_BASE_URL` |
| `llm.provider` | `""` (auto-detect) | `OVERMIND_LLM_PROVIDER` |
| `llm.model` | `claude-sonnet-4-5` | `OVERMIND_LLM_MODEL` |
| `llm.api_key_env` | `""` (per provider) | `OVERMIND_LLM_API_KEY_ENV` |
| `llm.base_url` | `""` (`https://api.openai.com/v1`) | `OVERMIND_LLM_BASE_URL` |
| `llm.cli_command` | `claude` | `OVERMIND_LLM_CLI_COMMAND` |
| `llm.cli_args` | `["-p", "--output-format", "json"]` | — |
| `llm.cli_timeout_sec` | `180` | `OVERMIND_LLM_CLI_TIMEOUT_SEC` |
| `db_path` | `~/.overmind/overmind.db` | `OVERMIND_DB_PATH` |
| `max_parallel` | `3` | `OVERMIND_MAX_PARALLEL` |
| `poll_interval_sec` | `15` | `OVERMIND_POLL_INTERVAL_SEC` |
| `task_timeout_min` | `45` | `OVERMIND_TASK_TIMEOUT_MIN` |
| `no_signal_timeout_min` | `10` | `OVERMIND_NO_SIGNAL_TIMEOUT_MIN` |

Use `--config <path>` to point at a different config file.

## LLM providers

The planner supports three providers, selected by `llm.provider`:

- `cli` — shells out to an installed coding-agent CLI in headless mode
  (default `claude -p --output-format json <prompt>`), reusing its stored
  login. No API key needed.
- `anthropic` — Anthropic Messages API. Needs an API key.
- `openai` — any OpenAI-compatible `/chat/completions` endpoint (OpenAI,
  DeepSeek, Ollama, ...) via `llm.base_url`. Needs an API key.

When `llm.provider` is empty, Overmind auto-detects: `cli` if the
`llm.cli_command` binary is in `PATH`, else `anthropic` if its API key env
var is set, else it errors with the available options.

`llm.api_key_env` is the **name** of the environment variable holding the
key — never the key itself. Key values are never logged.

OpenAI:

```yaml
llm:
  provider: openai
  model: gpt-4o
  api_key_env: OPENAI_API_KEY   # name of the env var, not the key
```

DeepSeek:

```yaml
llm:
  provider: openai
  model: deepseek-chat
  base_url: https://api.deepseek.com
  api_key_env: DEEPSEEK_API_KEY
```

Ollama (local, key value can be anything non-empty):

```yaml
llm:
  provider: openai
  model: llama3.1
  base_url: http://localhost:11434/v1
  api_key_env: OLLAMA_API_KEY
```

## Test

```sh
cd brain
go vet ./... && go test ./...
```
