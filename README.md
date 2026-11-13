<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Chartroom" height="56">
  </picture>
</p>

<p align="center">
  <strong>Publishes, links, merges and retires catalog entries.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: built, not deployed.** Chartroom runs and passes its tests alone, against in-memory stand-ins for Muster and Plainforge. It has never run against a real Muster or in a cluster, because neither exists yet. What is missing is listed under [Not yet done](#not-yet-done).

Chartroom is stage 08 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Chartroom is meant to be understood and used without the rest.

## What works today

- **The four rules.** Given a catalog and one decision, Chartroom works out the catalog change: publish a `building` item as `live`, link or merge a `scored` item, retire a `live` entry. The same input always gives the same bytes out.
- **Refusing instead of guessing.** Broken data is `E_PARSE`. Valid data with no rule is `E_NEEDS_SUBTOOL`. Neither changes the catalog.
- **The worker loop.** It leases items from Muster, most urgent first, stages each result in the spool directory, hands it to Muster and keeps trying with backoff until Muster acknowledges. A result staged before a restart is sent first after it.
- **Running alone.** An in-memory Muster and an in-memory hub repository stand in for the real ones, so the whole loop runs on one machine with nothing else.
- **Small and unprivileged.** The built binary peaks at about 18 MiB under a load of the largest items the contract allows, writes only to the spool directory, and needs no privileges. The image builds from source with `ko`; there is no Dockerfile.

It uses the Go standard library and nothing else. There is no model and no call to one.

## How to run it

You need Go 1.22 or later.

```sh
go test ./...                 # every test, including the saved examples
```

Apply one decision to a catalog file and print the change:

```sh
go run ./cmd/chartroom apply \
  -catalog  testdata/apply/ok-publish/catalog.json \
  -decision testdata/apply/ok-publish/decision.json
```

Add `-write` to rewrite the catalog file in place. Without it the file is not touched. A refused decision prints one line and exits 3 (`E_PARSE`) or 4 (`E_NEEDS_SUBTOOL`):

```
E_PARSE missing_provenance provenance: the provenance block is missing or incomplete
```

Run the whole worker alone, against the stand-ins, and print what happened to each item and the catalog that results:

```sh
mkdir -p /tmp/cs-spool
go run ./cmd/chartroom run -standalone \
  -catalog testdata/standalone/catalog.json \
  -items   testdata/standalone/items \
  -spool   /tmp/cs-spool
```

Run it as it runs in the cluster, against a Muster over HTTP:

```sh
chartroom run -muster-url http://… [-token-file FILE] [-spool /var/spool/cs] [-once]
```

Build the image and print the manifests with `ko`:

```sh
make image        # pushes to the in-cluster registry as …/chartroom
make manifests    # deploy/chartroom.yaml with the image reference filled in
```

## The saved examples

Every rule has a saved input and the exact output expected. A change in behaviour fails a test.

| Where | What |
| --- | --- |
| [`testdata/apply/`](testdata/apply) | 93 cases: a catalog and a decision in; the exact change and catalog out, or the exact error line. Run three ways: through the rules, through the command, and through the whole worker (in memory and over HTTP). |
| [`testdata/lease/`](testdata/lease) | 11 leases as Muster would send them, and the exact record the worker stages for each. |
| [`testdata/worker/`](testdata/worker) | The `E_TIMEOUT` example: the staged file and the log of a result that was retried after 1 s, 2 s and 4 s. |
| [`testdata/standalone/`](testdata/standalone) | Six items of mixed priority run through the whole loop, and the exact report. |

A test checks that every error reason Chartroom can give appears in at least one saved output. `make golden` rewrites the expected outputs from the code; read the diff before committing it.

## How the specification is met

| `SPEC.md` | Where |
| --- | --- |
| Four state transitions | `internal/decision`; a test pins the table to the specification. |
| Urgent before normal before backfill | The worker sorts each batch and the spool sends in that order. |
| Muster only, over HTTP, with the ServiceAccount token | `internal/muster`. The token is read fresh for each call. A test reads the import lists: only this package touches the network, and nothing in the worker reaches Plainforge. |
| Results staged in `/var/spool/cs` before Muster commits | `internal/spool`: written, synced and renamed into place, removed on acknowledgement. |
| `E_PARSE`: fails cleanly, sanitized report | The report is a fixed code, reason, field name and message. Nothing from the data is repeated. |
| `E_TIMEOUT`: stays staged, retries with backoff | 1 s doubling to 60 s, no random part. Leases with a staged result are kept alive by heartbeat. |
| `E_NEEDS_SUBTOOL`: fails closed | Unknown action, unknown schema version and every catalog state without a rule. |
| Provenance note preserved, no raw snapshots | The note is published byte for byte. The provenance block has two fields and any other is refused. |
| 30 MiB | A test runs the built binary under load and reads its peak memory. |
| Non-root, no added privileges, read-only root filesystem | [`deploy/chartroom.yaml`](deploy/chartroom.yaml), pinned by a test. Another test checks the binary writes nowhere but the spool. |
| No error names a machine, an address or a credential | Errors are constants. Tests check the HTTP client, the logs and the command for leaks. |

The wire format Chartroom was built against is in [`docs/contract.md`](docs/contract.md).

## Not yet done

- **Never run against a real Muster.** The five call paths and their bodies are a best reading and will change when Muster's specification exists.
- **Never run in a cluster.** The manifest has not been applied anywhere. The image was built with `ko` to a local layout and not pushed.
- **Priority class.** The specification says Chartroom runs under the Standard priority class and does not give the class's name. The manifest does not set one.
- **Network policy.** The policy allows connections to Muster and to cluster DNS only. It finds Muster by the label `app.kubernetes.io/name: muster`, which is a guess.
- **Muster's address.** The manifest reads it from a ConfigMap named `chartroom`, key `muster-url`, which nothing creates yet.
- **The five Shakedown conditions.** The specification gives the number and not the names. Chartroom checks there are five, each different, all passed.
- **Mirroring to GitHub.** Not built. See the first open question below.
- **The spool volume** is an `emptyDir`: it survives a container restart and not the deletion of the pod. Nothing is lost when that happens, because Muster leases the item again and the result is the same, but it is weaker than the specification's wording suggests.

## Where the specification needed a decision

These were raised before building. Each is a proposed change to `SPEC.md` or to the registry; none has been made there yet.

1. **Who writes to Plainforge.** The registry says Chartroom "writes catalog entries to the hub repository in Plainforge". `SPEC.md` says it talks only to Muster and is blocked from Plainforge. Built as `SPEC.md` has it: Chartroom hands the change to Muster and Muster commits it. *Proposed:* change the registry line to "Hands catalog entries to Muster, which commits them to the hub repository in Plainforge", and in the `E_TIMEOUT` row say "Muster, or the catalog sync behind it". The sentence about mirroring source repositories to GitHub is then clearly not Chartroom's job; say whose it is.
2. **The schema.** *Proposed:* add the item, catalog and change formats from [`docs/contract.md`](docs/contract.md), or point to where Muster defines them.
3. **Provenance on merged and retired entries.** The specification requires it for `live` and `linked` only. Built that way: a merge may carry it, a retirement keeps what the live entry had. *Proposed:* say so.
4. **Catalog states without a rule.** Retiring an entry that is not `live`, merging into a parent that is not `live`, publishing over an entry that differs: all pause as `E_NEEDS_SUBTOOL`. An exact repeat is a no-op. *Proposed:* add these to the failure table.
5. **Public links.** Chartroom refuses a catalog link that is not `https`, carries a user name or password, or points at an IP address or a private-network name. The specification does not ask for this. *Proposed:* add it to the security section.
6. **Numbers.** Lease batch (4), heartbeat (20 s), call timeout (10 s), backoff (1 s to 60 s) and poll interval (5 s) are defaults chosen here. *Proposed:* state them, or state that the worker chooses.
7. **"An existing free, private tool"** in the Link row is unclear. Built as a name and a public link.

## In this repository

| Path | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Chartroom does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt this was built from. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`docs/contract.md`](docs/contract.md) | The item, catalog and error formats, and the calls to Muster. |
| [`cmd/chartroom`](cmd/chartroom) | The command: `apply`, `run`, `version`. |
| [`internal/decision`](internal/decision) | The four rules. |
| [`internal/catalog`](internal/catalog) | The catalog file: strict reading, canonical writing, field rules. |
| [`internal/worker`](internal/worker), [`internal/spool`](internal/spool) | The loop and the on-disk staging. |
| [`internal/muster`](internal/muster) | The interface to Muster, its HTTP client and the in-memory stand-in. |
| [`internal/plainforge`](internal/plainforge) | The interface to the hub repository and its in-memory stand-in. Used by the Muster stand-in only. |
| [`internal/cserr`](internal/cserr) | The three error codes and every reason under them. |
| [`testdata/`](testdata) | The saved examples. |
| [`deploy/`](deploy), [`.ko.yaml`](.ko.yaml) | The Kubernetes manifest and the image build. |
| [`brand/`](brand) | The Chartroom logo and mark, for light and dark backgrounds. |

## At a glance

| | |
| --- | --- |
| **Stage** | 08 of 10 in the pipeline |
| **Formerly** | `cs-08-catalog`, then Gazette |
| **Namespace** | `cs-workers` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/chartroom` |
| **Memory budget** | 30 MiB |
| **Depends on** | [Muster](https://github.com/ClearedStack/muster), [Plainforge](https://github.com/ClearedStack/plainforge) |
| **Depended on by** | Nothing else in the platform |

### Boundaries

- Leases verified, linked, merged and retired items from Muster
- Writes catalog entries to the hub repository in Plainforge (through Muster; see the first open question above)

## Names

`chartroom` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
