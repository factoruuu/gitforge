# gitforge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/factoruuu/gitforge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/factoruuu/gitforge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single Python 3 program, standard library only — no pip packages, no external git binary. One process serves, from one shared in-memory store:

- **Git smart HTTP** — upload-pack protocol v2 (fetch + ls-refs, shallow clones, incremental fetch) and v0, receive-pack v0 with sideband report-status, per-ref non-fast-forward refusal, full pack reading (ofs/ref deltas) and writing, annotated tags, branch/tag deletion.
- **REST API** under `/api/v3` — repositories, contents (create/update/delete with sha checks), git refs/trees/commits, branches, labels, issues + comments, pull requests with file diffs and squash merges (real 3-way tree merges that land as commits on the default branch).
- **GraphQL** at `/api/graphql` — viewer/repository/issues/pullRequests queries with cursor pagination, introspection, and createIssue / closeIssue / createPullRequest / mergePullRequest / addComment mutations, all consistent with the REST view of the same objects.
- **MCP** at `/mcp` — JSON-RPC tools (create_repo, get_repo, create_issue, list_issues, create_pull_request, merge_pull_request) over the same store.

Auth is a single bearer token; the token's owner is the viewer. Storage is in-memory and deterministic (sequential ids, `MAKEFUN_TRIAL_NOW` pins timestamps).

Architecture: `main.py` puts its own directory on `sys.path` before importing the server (it runs as `python3 -P -s main.py`). `server.py` owns HTTP routing and the git/REST/GraphQL/MCP protocol handling; `store.py` holds repository, object, ref, issue and pull-request state. `demo.py` is an end-to-end proof with the real git CLI: it starts the forge, creates a repo over REST, pushes a commit, clones it back and verifies the file contents.

## Run it

```sh
python3 main.py
```

Standalone mode serves on port 8080 (set `PORT` and `MAKEFUN_SHOP_TOKEN` to customise; the token defaults to `dev-token`). In make.fun lifecycle mode, set `MAKEFUN_ENV_URL` and `MAKEFUN_SHOP_TOKEN`: the forge binds an ephemeral port, registers it with the environment, serves until the environment reports done, and exits 0. Verify the whole flow any time with `python3 demo.py`.

---
<sub>Entered on [make.fun](https://make.fun/?ref=afd1nwqx&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
