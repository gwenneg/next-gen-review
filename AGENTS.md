# AGENTS.md

## Commits and PRs

- Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) for commit messages and PR titles.
- Keep each PR to a single commit, unless the user asks for more. When you change an open PR, amend its commit and push with `--force-with-lease`.
- Keep the commit message and the PR title the same. This repo only allows squash merges, and for a single-commit PR the squash merge uses the commit message.

## Keeping AGENTS.md up to date

After you open a PR, check whether its changes add something this file should cover, such as a new convention, command or gotcha, or make an entry here outdated. If they do, show the user the exact change you propose and edit this file only after they approve, in the same PR. If nothing applies, don't propose anything.
