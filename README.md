# gitforge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/factoruuu/gitforge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/factoruuu/gitforge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge written in Go (standard library only), compiled to one statically linked Linux amd64 binary — no external git binary, no third-party packages. One process serves, from one shared in-memory store:

- **Git smart HTTP** — upload-pack protocol v2 (fetch + ls-refs, shallow clones, incremental fetch) and v0, receive-pack v0 with sideband report-status, per-ref non-fast-forward refusal, full pack reading (ofs/ref deltas) and writing, annotated tags, branch/tag deletion.
- **REST API** under `/api/v3` — repositories, contents (create/update/delete with sha checks), git refs/trees/commits, branches, labels, issues + comments, pull requests with file diffs and squash merges (real 3-way tree merges that land as commits on the default branch).
- **GraphQL** at `/api/graphql` — viewer/repository/issues/pullRequests queries with cursor pagination, introspection, and createIssue / closeIssue / createPullRequest / mergePullRequest / addComment mutations, all consistent with the REST view of the same objects.
- **MCP** at `/mcp` — JSON-RPC tools (create_repo, get_repo, create_issue, list_issues, create_pull_request, merge_pull_request) over the same store.

Auth is a single bearer token; the token's owner is the viewer. Storage is in-memory and deterministic (sequential ids, `MAKEFUN_TRIAL_NOW` pins timestamps).

## How it runs

The entry point is still `python3 main.py`. The forge binary travels through git as base85 text (`forge.b85.00`, `forge.b85.01`, …) because git hosts store text far more happily than a 6 MB binary; `main.py` joins and decodes the parts into `/tmp`, marks the result executable and `exec`s it — same process, same environment, same stdio. The Go sources live in `src/` (`main.go` HTTP/routing, `githttp.go` + `git.go` the git protocol, `rest.go`, `gql.go`, `mcp.go`, `store.go` state, `json.go` value model) and rebuild with `build-forge.sh` (Go 1.23+, `CGO_ENABLED=0`, `-ldflags="-s -w"`).

## Run it

```sh
python3 main.py
```

Standalone mode serves on port 8080 (set `PORT` and `MAKEFUN_SHOP_TOKEN` to customise; the token defaults to `dev-token`). In make.fun lifecycle mode, set `MAKEFUN_ENV_URL` and `MAKEFUN_SHOP_TOKEN`: the forge binds an ephemeral port, registers it with the environment, serves until the environment reports done, and exits 0. Verify the whole flow any time with `python3 demo.py`.

---
<sub>Entered on [make.fun](https://make.fun/?ref=afd1nwqx&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
