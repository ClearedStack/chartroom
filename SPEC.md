# Chartroom specification

> Catalog publication worker that publishes verified, linked, merged, and retired entries with provenance links to the clearedstack.com catalog.

**Status: design.** This is a specification. It describes how Chartroom is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/chartroom/](https://clearedstack.com/platform/chartroom/). To change the specification, change it here and say why.

## Overview & Role

Chartroom is the catalog publication worker in the `cs-workers` namespace (`ownerRepo: Chartroom`, version `v0.1.0-draft`). Split out of the original Stage 9 (List) alongside Shakedown, its job is to publish verified, linked, merged, and retired entries, complete with provenance links, to the `clearedstack.com` catalog (`/tools`).

Chartroom is a 100% deterministic Go service with zero AI at runtime. It never builds code or runs verification checks itself; it only publishes outcomes that have already passed Shakedown or been determined in Countersign.

## Runtime & Resource Budget

| Property | Value |
| --- | --- |
| **Service / Repo** | Chartroom (`chartroom`) |
| **Namespace** | `cs-workers` |
| **Kind** | `worker` (100% deterministic Go, zero runtime AI) |
| **Version** | `v0.1.0-draft` |
| **RAM Budget** | `30 MiB` |
| **Sub-Tools** | None (`[]`: uniform catalog publication logic) |
| **Local Spool** | `/var/spool/cs` (buffers publication acknowledgements if Muster restarts) |
| **Contract Version** | `CONTRACT_VERSION=v1` (owned by `clearedstack.com`) |

Chartroom runs under the Standard Kubernetes priority class, keeping baseline memory around `30 MiB` and yielding before High-priority control-plane pods on a single 4 GiB `k3s` node.

## Interfaces & State Transitions

Chartroom communicates exclusively with Muster over cluster-private HTTP using its Kubernetes ServiceAccount identity (`lease`, `heartbeat`, `complete`, `fail`, `needs_subtool`), processing `urgent` items before `normal` and `normal` before `backfill`. It has zero direct access to Logbook.

| Source State | Target State | Trigger & Catalog Action |
| --- | --- | --- |
| `building` | `live` | Shakedown passed all 5 conditions; publishes active tool entry and provenance links to `/tools`. |
| `scored` | `linked` | Owner in Countersign chose **Link**; publishes external reference to an existing free, private tool (avoiding duplicate rebuild). |
| `scored` | `merged` | Owner in Countersign chose **Merge**; publishes catalog note folding scope into the parent tool repository. |
| `live` | `retired` | Governing regulation repealed or superseded (detected by Tidegauge); archives active listing while retaining historical note. |

Completed publication payloads are staged in `/var/spool/cs` before Muster commits the state transition to `core.*` and refreshes `pub.*` views for Conn. Public tool source repositories in Plainforge are mirrored to GitHub on change (with zero container images pushed to GitHub).

## Sub-Tools & Failure Modes

Chartroom defines **no per-case sub-tools** (`subTools: []`) because catalog entry generation follows a single deterministic schema enforced by Muster.

| Error Code | Trigger | Pipeline Behavior |
| --- | --- | --- |
| `E_PARSE` | Missing provenance block, missing Shakedown pass attestation, or invalid catalog metadata | Fails cleanly without publishing; reports sanitized error to Muster for deduplication by Bellbuoy. |
| `E_TIMEOUT` | Muster or catalog sync endpoint times out | Result stays safely staged in `/var/spool/cs` and retries with backoff until acknowledged. |
| `E_NEEDS_SUBTOOL` | Unsupported catalog entry type or schema version mismatch | Fails closed, pauses item in `needs_subtool`, and awaits a deterministic update deployed via Shipkeeper. |

## Security & Clean-Room Boundaries

- **Zero direct database access:** Network policies block Chartroom from Logbook, Plainforge, and Lazarette; all state writes flow through Muster transactions.
- **Unprivileged pod posture:** Non-root user, dropped Linux capabilities, no privilege escalation, and read-only root filesystem outside `/var/spool/cs`.
- **Provenance & attribution:** Every published `live` or `linked` entry preserves the deterministic Go-template provenance note stamped by Waybill and carries no raw Sextant snapshots.
- **Sanitized observability:** Cluster internals are stripped by Shipkeeper before any worker health or error state appears on Conn.

## Related pages

- [The pipeline and its ten workers](https://clearedstack.com/platform/pipeline/#building-verifying-and-listing)
- [Clean room and credit](https://clearedstack.com/platform/clean-room-and-credit/)
- [An item’s life: fourteen states and three lanes](https://clearedstack.com/platform/item-lifecycle/)
- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
