# Muster contract v1: saved examples

The `.json` files in this directory are Muster's own saved examples of its contract, one for each call and each refusal. They are copied here unchanged so Chartroom's tests can replay them without the Muster repository.

| | |
| --- | --- |
| **From** | `contract/v1/examples/` in [ClearedStack/muster](https://github.com/ClearedStack/muster) |
| **Branch** | `claude/repo-build-prompt-1csgx6` |
| **Commit** | `a362cb1051448d496b5b97ff808ebda60370502f` |
| **License** | MIT, the same as this repository |

The format of a file is described in the `README.md` beside the originals. The contract itself is `contract/v1/CONTRACT.md` in the same place.

## What the tests do with them

`internal/muster` replays every example whose request is one of the five worker calls:

- Chartroom's HTTP client is made to send the same call. The body it sends must equal the example's request, field for field and in the same order.
- The client is given the example's answer, status and body exactly as saved, and must read it the way the contract means it.
- Where an example needs nothing set up first, its request is also sent as it is to Chartroom's in-memory stand-in for Muster, which must give the saved answer exactly.

Examples of calls that are not a worker's (`/v1/items`, `/v1/edits`, `/v1/feed`, `/v1/health`, `/v1/release`, `/healthz`) are not Chartroom's to make and are skipped by path.

## Updating

Copy `contract/v1/examples/*.json` here again, change the commit above, and run `go test ./internal/muster`. A failure is a change in the contract that Chartroom has to follow.
