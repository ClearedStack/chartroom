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

> **Status: built, not deployed.** Chartroom runs and passes its tests alone, against in-memory stand-ins for Muster and Plainforge. It speaks Muster's contract v1 and replays that contract's saved examples, but it has never run against a real Muster or in a cluster. What is missing is listed under [Not yet done](#not-yet-done).

Chartroom is stage 08 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Chartroom is meant to be understood and used without the rest.

## What works today

- **The four rules.** Publish a `building` item as `live`; list a `linked` item, a `merged` item, a `retired` item. Each turns what earlier workers recorded into one catalog entry. The same input always gives the same bytes out.
- **Refusing instead of guessing.** Broken data is `E_PARSE`. Valid data with no rule is `E_NEEDS_SUBTOOL`. Neither publishes anything.
- **Muster's contract v1.** One lease per call, the five worker calls with exactly the fields the contract lists, refusals read by status and cause. Muster's own saved examples are replayed in the tests.
- **The worker loop.** It leases a step, stages the result in the spool directory, hands it to Muster and keeps trying with backoff until Muster answers. A result staged before a restart is sent first after it.
- **Running alone.** An in-memory Muster and an in-memory hub repository stand in for the real ones, so the whole loop runs on one machine with nothing else.
- **Small and unprivileged.** The built binary peaks at about 18 MiB under a load of the largest entries the contract allows, writes only to the spool directory, and needs no privileges. The image builds from source with `ko`; there is no Dockerfile.

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

Run the whole worker alone, against the stand-ins. Each file under `-items` is an item as Muster would hold it: `id`, `state`, `lane` and the `results` of earlier workers. It prints what happened to each item and the catalog that results:

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
| [`testdata/apply/`](testdata/apply) | 98 cases: a catalog and a decision in; the exact change and catalog out, or the exact error line. Run through the rules, through the command and through the built binary. |
| [`testdata/lease/`](testdata/lease) | 32 lease answers in the shape of Muster's contract, and the exact record the worker stages for each. Every one that is an item is also run through the whole worker, in memory and over HTTP. |
| [`testdata/worker/`](testdata/worker) | Two logs: a result retried after 1 s, 2 s and 4 s (`E_TIMEOUT`), with the staged file; and a pause that Muster refuses. |
| [`testdata/standalone/`](testdata/standalone) | Six items in three lanes run through the whole loop, and the exact report. |
| [`testdata/muster-contract/`](testdata/muster-contract) | Muster's own 50 saved examples of its contract, copied unchanged. Every worker call and every refusal among them is replayed against Chartroom's client, and those that need no setup against its stand-in. |

A test checks that every error reason Chartroom can give appears in at least one saved output. `make golden` rewrites the expected outputs from the code; read the diff before committing it.

## How the specification is met

| `SPEC.md` | Where |
| --- | --- |
| Publish, link, merge, retire | `internal/decision`; a test pins the four steps to Chartroom's rows in Muster's contract. See the first open question below for where that differs from the table in `SPEC.md`. |
| Urgent before normal before backfill | Muster chooses the order and gives one step per lease call. The worker takes the steps as they come and sends each result before asking for the next. Results staged across a restart are sent in lane order. |
| Muster only, over HTTP, with the ServiceAccount token | `internal/muster`. The token is read fresh for each call. A test reads the import lists: only this package touches the network, and nothing in the worker reaches Plainforge. |
| Results staged in `/var/spool/cs` before Muster commits | `internal/spool`: written, synced and renamed into place, removed when Muster answers. |
| `E_PARSE`: fails cleanly, sanitized report | Muster is sent the code and nothing else. The reason and the field name go to Chartroom's log; nothing from the data is repeated. |
| `E_TIMEOUT`: stays staged, retries with backoff | 1 s doubling to 60 s, no random part. Leases with a staged result are kept by heartbeat. |
| `E_NEEDS_SUBTOOL`: fails closed | Unknown state, a step that is not an item, a block found twice, a retirement with nothing to retire. Nothing is published. See the second open question for what Muster then does. |
| Provenance note preserved, no raw snapshots | The note is published byte for byte. Only named blocks are read from earlier results, each has a closed set of fields, and nothing else is copied. |
| 30 MiB | A test runs the built binary under load and reads its peak memory. |
| Non-root, no added privileges, read-only root filesystem | [`deploy/chartroom.yaml`](deploy/chartroom.yaml), pinned by a test. Another test checks the binary writes nowhere but the spool. |
| No error names a machine, an address or a credential | Errors are constants. Tests check the HTTP client, the logs and the command for leaks. |

Both contracts Chartroom works to are in [`docs/contract.md`](docs/contract.md).

## Not yet done

- **Never run against a real Muster.** The client follows Muster's contract v1 and its saved examples as of the commit named in [`testdata/muster-contract/README.md`](testdata/muster-contract/README.md). It has only ever talked to Chartroom's own stand-in.
- **Never run in a cluster.** The manifest has not been applied anywhere. The image was built with `ko` to a local layout and not pushed.
- **Where the entry's content comes from.** No specification says which earlier worker records the slug, the title, the repository, the provenance or the attestation, or in what shape. Chartroom reads named blocks from earlier results ([`docs/contract.md`](docs/contract.md)). Until the workers before it agree to write them, every real item will fail with `E_PARSE`.
- **Priority class.** The specification says Chartroom runs under the Standard priority class and does not give the class's name. The manifest does not set one.
- **Network policy.** The policy allows connections to Muster and to cluster DNS only. It finds Muster by the label `app.kubernetes.io/name: muster`, which is a guess.
- **Muster's address.** The manifest reads it from a ConfigMap named `chartroom`, key `muster-url`, which nothing creates yet.
- **The five Shakedown conditions.** The specification gives the number and not the names. Chartroom checks there are five, each different, all passed.
- **Mirroring to GitHub.** Not built. It reads as another component's job.
- **The spool volume** is an `emptyDir`: it survives a container restart and not the deletion of the pod. Nothing is lost when that happens, because Muster offers the step again and the result is the same, but it is weaker than the specification's wording suggests.

## Where the specifications needed a decision

Each of these is a proposed change to `SPEC.md`, to the registry or to Muster's contract. None has been made there.

1. **The state table.** `SPEC.md` has Chartroom move an item from `scored` to `linked` or `merged` and from `live` to `retired`. Muster's contract has Countersign and Tidegauge make those moves, and Chartroom then list an item that is already `linked`, `merged` or `retired`, leaving the state as it is. Built as Muster has it, since Muster refuses anything else. *Proposed:* change the table in `SPEC.md` to Chartroom's four rows in Muster's contract.
2. **Pausing.** `SPEC.md` says an item with no rule is paused in `needs_subtool`. Muster's state table allows a pause only from `recorded`, `observed` and `approved`, none of which Chartroom ever holds, so every `needs_subtool` call from Chartroom is refused with `move_not_allowed`. Chartroom makes the call, drops the result when it is refused and publishes nothing. The item is then offered again each time its lease runs out, with the same end. *Proposed:* let Muster pause from `building`, `linked`, `merged` and `retired`, or say what Chartroom should do instead.
3. **What Muster learns of a failure.** `SPEC.md` says Chartroom reports a sanitized error for Bellbuoy to deduplicate. Muster's `fail` call carries one of two codes and nothing else. The reason stays in Chartroom's log. *Proposed:* say so, or give `fail` a field for a fixed reason.
4. **Who writes to Plainforge.** The registry says Chartroom "writes catalog entries to the hub repository in Plainforge". `SPEC.md` says it talks only to Muster. Built as `SPEC.md` has it: the entry is the `result` of the `complete` call, and Muster keeps it. Nothing yet says who carries it from there to the hub repository. Chartroom's stand-in for Muster does it, so the tests have a catalog to read. *Proposed:* change the registry line, and name the component that writes the hub repository.
5. **The content of an entry.** See "Where the entry's content comes from" above. *Proposed:* add the blocks in [`docs/contract.md`](docs/contract.md) to the specifications of the workers that would supply them. In particular, `SPEC.md` says the provenance note is stamped by Waybill, but in Muster's table a linked item never reaches Waybill.
6. **What the worker cannot see.** Muster hands over one item, not the catalog. So the worker cannot check that a merge's parent is a live entry, or that a slug is not already used by another item. The `apply` command, which is given the whole catalog, checks both. *Proposed:* say which component makes those checks.
7. **Public links.** Chartroom refuses a catalog link that is not `https`, carries a user name or password, or points at an IP address or a private-network name. The specification does not ask for this. *Proposed:* add it to the security section.
8. **Numbers.** Heartbeat (20 s), call timeout (10 s), backoff (1 s to 60 s) and poll interval (5 s) are defaults chosen here. *Proposed:* state them, or state that the worker chooses.

## In this repository

| Path | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Chartroom does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt this was built from. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`docs/contract.md`](docs/contract.md) | The calls to Muster, what Chartroom reads from earlier workers, what it sends back, and the errors. |
| [`cmd/chartroom`](cmd/chartroom) | The command: `apply`, `run`, `version`. |
| [`internal/decision`](internal/decision) | The four rules. |
| [`internal/catalog`](internal/catalog) | The catalog file: strict reading, canonical writing, field rules. |
| [`internal/worker`](internal/worker), [`internal/spool`](internal/spool) | The loop and the on-disk staging. |
| [`internal/muster`](internal/muster) | The interface to Muster, its HTTP client, the in-memory stand-in and the replay of Muster's contract examples. |
| [`internal/plainforge`](internal/plainforge) | The interface to the hub repository and its in-memory stand-in. Used by the Muster stand-in only. |
| [`internal/cserr`](internal/cserr) | The three error codes and every reason under them. |
| [`testdata/`](testdata) | The saved examples, Chartroom's own and Muster's. |
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
- Writes catalog entries to the hub repository in Plainforge (through Muster; see the fourth open question above)

## Names

`chartroom` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
