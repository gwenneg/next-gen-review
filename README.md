# next-gen-review

A tool that prepares your merge decision on pull requests. The AI never decides: it gathers the evidence, says what's settled and what isn't, and asks you only what it can't settle. `next-gen-review` is a working name.

Status: early skeleton. The stack starts, but there's no review feature yet.

## What you need on your computer

To run the stack:

| Tool | Notes |
|---|---|
| Docker Engine | Tested with Colima. Docker Desktop or Docker Engine on Linux should also work. Podman is not tested. |
| Docker Compose | A recent version: `make dev` uses `up --watch` and the `sync+restart` watch action, which older Compose 2.x versions don't have. |
| Docker Buildx | Recommended, not required. Without it, Docker uses the old builder, which builds every Dockerfile stage and is much slower: in one test, a clean api image build took 81s instead of 8s. |
| `make` | Preinstalled on macOS and most Linux distributions. |
| Claude Code | On your computer, only to run `claude setup-token`. It needs a Claude subscription. The worker image has its own copy. |

Go and Node are not needed to run the stack: the builds happen in containers. You need them for `make test` and for editor support:

| Tool | Version |
|---|---|
| Go | 1.26 or later |
| Node.js and npm | 26 (current LTS) |

Ports `8080` and, in dev mode, `5433` must be free on `127.0.0.1`.

Tested on macOS (Apple silicon) with Colima 0.10, Docker 29, Compose 5.5, Buildx 0.39, Go 1.26 and Node 26.

### macOS with Homebrew

Homebrew's `docker-compose` and `docker-buildx` aren't found by the Docker CLI until you add their folder to `~/.docker/config.json`:

```json
"cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]
```

Without it, `docker compose` and `docker buildx` fail with "unknown command". The Makefile still works, because it falls back to `docker-compose`.

## Run it on your laptop

1. Create your `.env`:
   ```sh
   cp .env.example .env
   ```
   Set `POSTGRES_PASSWORD` to any value. `CLAUDE_CODE_OAUTH_TOKEN` is only needed for `make check-claude` for now.
2. Start the stack and open http://127.0.0.1:8080:
   ```sh
   make dev   # hot reload, stays in the foreground
   make up    # or: production-like images, in the background
   make down  # stops either one, and keeps the database
   ```

Both dots at the top of the page turn green when the api, the database and the live updates work.

## Services

| Service | What it does |
|---|---|
| `db` | Postgres. |
| `api` | Go. HTTP API and WebSocket for live updates. Runs the database migrations at startup. |
| `worker` | Go. Will run the AI reviews with the Claude CLI. It gets the Claude token and nothing else. |
| `web` | React + TypeScript (Vite). Served by nginx, which also forwards `/api` to the api. |

Only `web` is published, on `127.0.0.1:8080`. In dev mode, Postgres is also on `127.0.0.1:5433`.

## Check your Claude token

```sh
claude setup-token        # on your laptop, then put the token in .env
make check-claude         # one tiny claude -p call from the worker image
```

## Tests

```sh
make test
```
