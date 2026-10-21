# Operations

Run one Go service with SQLite on a local persistent disk and a private ComfyUI instance. Compose runs both services on CPU. See [README.md](README.md) for setup and examples.

## API

`/v1/*` and `/metrics` require a bearer token. Local commands can use `curl --header @.data/auth-header`.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | /healthz | Process health |
| GET | /readyz | SQLite and ComfyUI availability |
| POST | /v1/jobs | Submit with an `Idempotency-Key` header |
| GET | /v1/jobs/{id} | Status and image count |
| GET | /v1/jobs/{id}/images/{index} | Download PNG |
| DELETE | /v1/jobs/{id} | Cancel queued work |
| POST | /v1/jobs/{id}/reconcile | Recover an uncertain job from upstream history |
| POST | /v1/jobs/{id}/abandon | Mark an uncertain job failed without stopping upstream work |
| GET | /metrics | Prometheus job counts by state |

Submission returns 202 for a new job, 200 for a replay, 409 for a reused key with different settings, or 429 at capacity. Keys must contain 8..128 ASCII letters, digits, periods, underscores, colons or hyphens. Output 0 is original; output 1 is adjusted/formatted.

## Configuration

`API_TOKEN` is required (32..256 characters). `make setup` creates local credentials; `make run` loads them for that process only.

| Variable | Default |
| --- | --- |
| ADDR | 127.0.0.1:8088 |
| COMFY_URL | http://127.0.0.1:8188 |
| DB_PATH | .data/jobs.db |
| WORKFLOW_PATH | workflow.json |
| MAX_QUEUE | 32 |
| MAX_JOBS | 1000 |
| MAX_RESULT_BYTES | 536870912 |
| JOB_TIMEOUT | 30m |

One worker processes jobs. Terminal jobs expire after 30 days; their idempotency keys expire with them. Uncertain jobs count toward queue capacity and require explicit recovery. ComfyUI output files need separate retention; the database quota does not cover them.

## Recovery

- ComfyUI offline before submission: jobs stay queued until it returns or the job deadline expires.
- Submission outcome unknown: the job becomes `uncertain`. Use `reconcile` to check upstream history. It never resubmits the prompt.
- Result cannot be recovered: use `abandon` to release capacity. Never reset uncertain jobs to queued in SQL; that can duplicate generation.
- Service restart: running jobs resume status checks; interrupted submissions become uncertain.

Stop with `make down` (Docker) or Ctrl-C/SIGTERM (native). The service stops accepting jobs and drains HTTP handlers. Keep the database on persistent storage. Back up with SQLite's `.backup` command; do not copy only the live `.db` file while WAL is active.

For deployment, inject the token, put TLS at a reverse proxy and keep ComfyUI private. One token grants access to all jobs. Readiness checks connectivity, not model or GPU readiness.
