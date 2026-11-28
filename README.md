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
| Go | 1.26.7 or later (an older Go 1.26 downloads it automatically) |
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
   - `POSTGRES_PASSWORD`: any value.
   - `GITHUB_TOKEN` (or `GH_TOKEN`, like the `gh` CLI): a fine-grained token with read-only access to public repositories, to list the PRs waiting for you. `.env.example` says how to create it. A `GH_TOKEN` or `GITHUB_TOKEN` exported in your shell wins over `.env`, so check that it's the token you mean: the page shows which variable it came from.
   - `CLAUDE_CODE_OAUTH_TOKEN`: the long-lived token of your Claude subscription, created with `claude setup-token`. Needed to run reviews.
2. Start the stack and open http://127.0.0.1:8080:
   ```sh
   make dev   # hot reload, stays in the foreground
   make up    # or: production-like images, in the background
   make down  # stops either one, and keeps the database
   ```

Both dots at the top of the page turn green when the api, the database and the live updates work.

## The queue

The page lists the open PRs in public repos that are waiting for you:

- **Review requested:** someone asked for your review, directly or through one of your teams, in any repo.
- **Watched repo:** a PR in a repo you added on the page, not a draft, not yours, and not reviewed by you yet. Watching is meant for repos with fewer than about 50 open PRs: every open PR that fits these rules is listed.
- **New commits since your review:** a PR you reviewed, where the author pushed commits after your last review.

The api checks GitHub every 2 minutes, and when you click Refresh. The page updates on its own when the list changes. Listing PRs uses no AI tokens.

## Services

| Service | What it does |
|---|---|
| `db` | Postgres. |
| `api` | Go. HTTP API and WebSocket for live updates. Runs the database migrations at startup. |
| `worker` | Go. Runs the AI reviews with the Claude CLI. It gets the Claude token and nothing else: its jobs come from the api's internal port, and the code from a read-only volume. |
| `web` | React + TypeScript (Vite). Served by nginx, which also forwards `/api` to the api. |

Only `web` is published, on `127.0.0.1:8080`. In dev mode, Postgres is also on `127.0.0.1:5433`.

## Reviews

Each PR in the list has a **Review** button. Pick the model (Sonnet by default), the effort, a token cap, and optionally some context for the AI, then start it. Reviews run one at a time, with your Claude subscription through the `claude` CLI, never the paid API:

1. The api downloads the PR's code at the commit shown in the list, into a volume the worker can only read.
2. The worker runs `claude -p` in that folder. Claude can only use Read, Grep and Glob, and can't read anything outside the folder.
3. The page shows the progress and the token count (cache reads are not counted). Cancel stops the review at once, and so does reaching the cap.
4. The review appears under the PR. The code is deleted, and the Claude session is kept in the worker's volume.

The worker stops a review right away if `claude` reports that it uses an API key instead of your subscription token, and it never passes API key variables to `claude`.

### Follow-up questions

A finished review shows a command to continue it in a terminal:

```sh
make continue REVIEW=12   # in the repo folder, with the stack running
```

It downloads the PR's code again, at the reviewed commit, and opens the review's claude session in the worker container, with the same read-only tools. Ask what you want, then quit claude (`/exit`): the code is deleted again. It uses your Claude subscription, and the session's earlier messages count again.

- The first time, claude shows its own setup screens (theme, security notes).
- For each review, claude first asks whether you trust the folder. Choose Yes: the review's flags still ignore the PR's settings, hooks, CLAUDE.md and MCP servers, and claude can only read files, in that folder only. It doesn't ask again for that review.
- If you close the terminal without quitting claude, the session keeps running in the container. The next `make continue` of that review stops it first.

## Check your Claude token

```sh
claude setup-token        # on your laptop, then put the token in .env
make check-claude         # one tiny claude -p call from the worker image
```

## Tests

```sh
make test
```
