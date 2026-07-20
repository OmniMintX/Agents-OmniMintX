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
| `llm.provider` | `anthropic` | `OVERMIND_LLM_PROVIDER` |
| `llm.model` | `claude-sonnet-4-5` | `OVERMIND_LLM_MODEL` |
| `llm.api_key_env` | `ANTHROPIC_API_KEY` | `OVERMIND_LLM_API_KEY_ENV` |
| `db_path` | `~/.overmind/overmind.db` | `OVERMIND_DB_PATH` |
| `max_parallel` | `3` | `OVERMIND_MAX_PARALLEL` |
| `poll_interval_sec` | `15` | `OVERMIND_POLL_INTERVAL_SEC` |
| `task_timeout_min` | `45` | `OVERMIND_TASK_TIMEOUT_MIN` |
| `no_signal_timeout_min` | `10` | `OVERMIND_NO_SIGNAL_TIMEOUT_MIN` |

Use `--config <path>` to point at a different config file.

## Test

```sh
cd brain
go vet ./... && go test ./...
```
