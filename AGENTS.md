# AGENTS.md

## Commits and PRs

- Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) for commit messages and PR titles.
- Keep each PR to a single commit, unless the user asks for more. When you change an open PR, amend its commit and push with `--force-with-lease`.
- Keep the commit message and the PR title the same. This repo only allows squash merges, and for a single-commit PR the squash merge uses the commit message.

## Development

- Run `make test` before pushing. `make dev` starts the stack with hot reload, and `make up` with built images. Both use http://127.0.0.1:8080.
- `make test` needs Docker running: the store and event tests start Postgres with the image from `compose.yaml`.
- Only the api runs database migrations. Add new ones in `backend/internal/db/migrations/` (goose).
- The worker only gets the Claude token. Never give it the GitHub token or the database password. claude always runs with `worker.Flags` (`--tools Read,Grep,Glob --restricted --safe-mode --strict-mcp-config`), the review and `make continue` alike: the PR's repo must not load its own settings, hooks or CLAUDE.md.
- Keep the api's `localOnly` Host check, the WebSocket Origin check and `http.CrossOriginProtection`. Never set `InsecureSkipVerify`.
- Only public repos: every GitHub search includes `is:public`, and private results are dropped in code too.
- Every comment the app posts to GitHub ends with the AI label (`review.CommentBody`, `review.MainBody`). Post only with `GH_REVIEW_TOKEN`: the queue token stays read-only. Never retry a POST to GitHub: a retried post can post twice.
- Call GitHub through `cli/go-gh`, not raw HTTP. Before writing code a module could cover, look for one, preferring the module maintained by the service's owner, at its latest release.

## Keeping AGENTS.md up to date

After you open a PR, check whether its changes add something this file should cover, such as a new convention, command or gotcha, or make an entry here outdated. If they do, show the user the exact change you propose and edit this file only after they approve, in the same PR. If nothing applies, don't propose anything.
