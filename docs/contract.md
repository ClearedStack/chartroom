# The contracts Chartroom works to

Two contracts meet in Chartroom.

1. **Muster's contract v1** says how a worker leases a step and reports on it. Muster owns it: `contract/v1/CONTRACT.md` in the [Muster repository](https://github.com/ClearedStack/muster). Chartroom follows it exactly and replays its saved examples in its own tests ([`testdata/contract/`](../testdata/contract)).
2. **What Chartroom needs to publish an entry** is not in any specification yet. Muster's contract says each worker's result is a JSON object and no more. This page is what Chartroom was built against, written down so it can be reviewed, corrected and moved into the specifications.

All data in the examples is made up.

## Talking to Muster

Five calls. Each is an HTTP `POST` with a JSON body, `Content-Type: application/json` and `Authorization: Bearer <ServiceAccount token>`. The token is read from its file for every call. Every body carries `"contract": "v1"` and has exactly the fields below, in this order; Muster refuses any other.

| Call | Path | Body Chartroom sends |
| --- | --- | --- |
| lease | `/v1/lease` | `{"contract": "v1"}` |
| heartbeat | `/v1/heartbeat` | `{"contract": "v1", "lease": "PA-0101.8.1"}` |
| complete | `/v1/complete` | `{"contract": "v1", "lease": "PA-0101.8.1", "state": "live", "result": { … }}` |
| fail | `/v1/fail` | `{"contract": "v1", "lease": "PA-0101.8.1", "code": "E_PARSE"}` |
| needs_subtool | `/v1/needs_subtool` | `{"contract": "v1", "lease": "PA-0101.8.1"}` |

One lease call gives one step, or `204` when there is none. A lease is a string Chartroom hands back exactly as it was given. Muster chooses the order: the `urgent` lane first, then `normal`, then `backfill`. Chartroom takes the steps as they come and sends each result before it asks for the next.

A lease answer is read strictly. It may hold `lease`, `kind`, `id`, `state`, `lane`, `attempt`, `expires`, `found`, `results`, `proposal` and `incident`, and nothing else.

### Chartroom's steps

These are Chartroom's rows in the contract's table of queue steps. The state Chartroom is handed decides what it does.

| Held in | Chartroom | Completes to |
| --- | --- | --- |
| `building` | publishes the tool | `live` |
| `linked` | lists the link | `linked` |
| `merged` | lists the merge | `merged` |
| `retired` | lists the retirement | `retired` |

Only the first changes the state. Countersign has already moved a linked or merged item, and Tidegauge a retired one, before Chartroom sees it. Any other state has no rule and is `E_NEEDS_SUBTOOL`. So is a step whose `kind` is not `item`.

### How Chartroom reads an answer

| Answer | Meaning | What Chartroom does |
| --- | --- | --- |
| `200` with the call's `CONTRACT.md` fields; `204` to a lease call | Acknowledged | Removes the result from the spool. |
| `409` `lease_not_held` | The lease ended or the step was taken over | Drops the result. Muster offers the step again if it still needs doing. |
| `409` `move_not_allowed` | Muster's tables do not allow it | Drops the result and logs it. |
| `422` `unknown_contract` | Muster does not speak `v1` | Drops the result and logs `E_NEEDS_SUBTOOL`. |
| Any other `4xx` refusal | The same call would be refused again | Drops the result and logs `E_PARSE`. |
| `429`, `5xx`, a timeout, no connection, an answer (including a malformed `200`) that is not the contract's | Not acknowledged | Keeps the result staged and sends it again after 1 s, 2 s, 4 s and so on, up to 60 s. |

A refusal is trusted only if it has the contract's exact shape: `error`, `cause`, `reason` and nothing else. Chartroom acts on the status and the `cause`. It never logs or repeats the `reason`. A lease call that is refused for any reason is tried again after the same doubling wait.

While a result waits in the spool, its lease is kept with a heartbeat every 20 seconds. Chartroom never follows a redirect and never uses a proxy. A request over Muster's 64 KiB limit is not sent.

### What reaches Muster when a step is not completed

`fail` carries the code and nothing else, and `needs_subtool` carries no code at all. The reason and the field stay in Chartroom's log. Chartroom never sends `fail` with `E_TIMEOUT`: nothing but Muster itself can be slow for it.

## What Chartroom needs from earlier workers

A lease on an item carries `results`: the latest result of each earlier worker, by worker name. Chartroom finds what it publishes there, as **blocks**: named fields at the top level of a worker's result.

```json
"results": {
  "waybill":   { "slug": "sample-clause-finder",
                 "entry": { "title": "Sample Clause Finder", "summary": "One line about the tool." },
                 "provenance": { "note": "Stamped upstream, published byte for byte.",
                                 "links": ["https://rules.example/part-900/section-12"] } },
  "moldloft":  { "publish": { "repository": "https://forge.example/tools/sample-clause-finder" } },
  "shakedown": { "attestation": { "conditions": [ { "name": "tests-pass", "passed": true } ] } }
}
```

The rule for finding a block:

- A block must be in **exactly one** earlier worker's result. Chartroom does not assume which worker: no specification says who supplies the title, and the provenance of a linked item cannot come from Waybill, which never sees one.
- A block in **no** result is missing: `E_PARSE`.
- A block in **two** results has no rule to choose between them: `E_NEEDS_SUBTOOL`.
- Everything else in a result is that worker's business. Chartroom does not read it, copy it or publish it.
- `results.chartroom` is Chartroom's own earlier result for the same item: the entry it published before. It is never searched for blocks.

### Blocks by step

| Step | Blocks read | Notes |
| --- | --- | --- |
| publish (`building`) | `slug`, `entry`, `publish`, `provenance`, `attestation` | All required. |
| link (`linked`) | `slug`, `entry`, `link`, `provenance` | All required. |
| merge (`merged`) | `slug`, `entry`, `merge`, `provenance` | `provenance` is optional. |
| retire (`retired`) | `retire` | The entry to retire is `results.chartroom`. If there is none, or it is not `live`, there is no rule: `E_NEEDS_SUBTOOL`. |

Blocks for other steps may still be in the results from earlier in the item's life. They are not read.

### The blocks

| Block | Rule |
| --- | --- |
| `slug` | Lower-case letters and digits in groups joined by single hyphens, at most 64 bytes. |
| `entry.title` | 1 to 120 characters, one line, no control characters, no white space at the ends. |
| `entry.summary` | 1 to 600 characters, same rules. |
| `publish.repository` | A public link. |
| `link.name`, `link.url` | A title, and a public link. |
| `merge.parent`, `merge.note` | Another slug, and a note. |
| `retire.reason`, `retire.note` | `repealed` or `superseded`, and a note. |
| `provenance.note` | Not blank, at most 4000 bytes. Published exactly as received. |
| `provenance.links` | 1 to 16 public links. |
| `attestation.conditions` | Exactly five, each with its own name, every one `passed: true`. |

A note is 1 to 2000 characters and may hold line breaks. A **public link** is an absolute `https` URL with no user name or password, whose host is a lower-case dotted DNS name: not an IP address, not a single label, and not ending in `.local`, `.internal`, `.localhost` or `.svc`.

Blocks are read strictly: invalid UTF-8, a duplicate key, a field not listed here or a key in another letter case is `E_PARSE`. The provenance block has exactly two fields, so a raw snapshot cannot be carried through it.

## What Chartroom sends back

The `result` of a `complete` call is the catalog entry and nothing else:

```json
{
  "slug": "sample-clause-finder",
  "status": "live",
  "title": "Sample Clause Finder",
  "summary": "One line about the tool.",
  "repository": "https://forge.example/tools/sample-clause-finder",
  "provenance": { "note": "…", "links": ["https://rules.example/part-900/section-12"] }
}
```

`status` is `live`, `linked`, `merged` or `retired`. A `linked` entry has `link` in place of `repository`. A `merged` entry has `merge` and neither. A `retired` entry is the live entry as it was, with `status` changed and a `retirement` block (`reason`, `note`) added: that block is the historical note.

Muster keeps the result as `results.chartroom`, replacing the one before. So a publish, link or merge always sends the whole new entry, and a retirement starts from the entry kept there. An entry carries nothing else from the lease: no attestation, no item identifier, no state, nothing from any other worker.

## The `apply` command's files

`chartroom apply` works on files instead of a lease, and on a whole catalog instead of one item.

A **decision file** is the item as one document:

```json
{
  "contract": "v1",
  "id": "PA-0101",
  "state": "building",
  "action": "publish",
  "slug": "sample-clause-finder",
  "entry": { "title": "…", "summary": "…" },
  "publish": { "repository": "https://…" },
  "provenance": { "note": "…", "links": ["https://…"] },
  "attestation": { "conditions": [ … ] }
}
```

`id` has the form of Muster's item identifiers: `PA-` and four digits, or five to nine digits with no leading zero. `action` is `publish`, `link`, `merge` or `retire`, and `state` must be the state that step is held in. In a file, a block that belongs to another action is `E_PARSE`. The whole file is at most 64 KiB.

A **catalog file** is `{"contract": "v1", "entries": [ … ]}`, written the same way every time: entries in slug order, two-space indent, no HTML escaping, one trailing newline.

Because it sees the whole catalog, `apply` checks two things the worker cannot:

| Catalog before | Result |
| --- | --- |
| An entry with this slug that differs from what the item would make | `E_NEEDS_SUBTOOL`. It may belong to another item. |
| A merge whose parent slug is not a `live` entry | `E_NEEDS_SUBTOOL` |

An item whose result is already in the catalog changes nothing (`"changed": false`).

It prints the **change**: `contract`, `id`, `action`, `from`, `to`, `slug`, `changed`, and the entry `before` and `after`.

## Errors

Three codes, each with a fixed list of reasons. Every message is a constant; nothing from the data, the network or the operating system is copied into one. `field` is the name of a field in these contracts, never a value.

| Code | Reason | When |
| --- | --- | --- |
| `E_PARSE` | `malformed_json` | Not well-formed JSON, not an object, wrong type, duplicate key, invalid UTF-8, trailing data, too large. Also an answer from Muster that cannot be read. |
| `E_PARSE` | `unknown_field` | A field that is not in the contract. |
| `E_PARSE` | `invalid_metadata` | A field is missing or breaks its rule, or a block is missing. |
| `E_PARSE` | `missing_provenance` | The provenance block is absent, or has a blank note or no links. |
| `E_PARSE` | `missing_attestation` | The attestation is absent or does not show five distinct passed conditions. |
| `E_PARSE` | `invalid_catalog` | The catalog, or Chartroom's own earlier entry, cannot be read or has the wrong shape. |
| `E_PARSE` | `upstream_refused` | Muster refused a call in a way that sending it again cannot cure. |
| `E_NEEDS_SUBTOOL` | `unsupported_entry_type` | A step that is not an item, an action or an entry status this build does not know. |
| `E_NEEDS_SUBTOOL` | `schema_version_mismatch` | A contract version other than `v1`: in a file, in `CONTRACT_VERSION`, or as Muster's answer. |
| `E_NEEDS_SUBTOOL` | `unsupported_transition` | The item is in a state Chartroom holds no item in. |
| `E_NEEDS_SUBTOOL` | `unsupported_catalog_state` | The catalog is in a state the action has no rule for. |
| `E_NEEDS_SUBTOOL` | `unsupported_value` | A retirement reason or a lane this build does not know, or a block found in two results. |
| `E_TIMEOUT` | `upstream_timeout` | Muster did not acknowledge a call. |

Printed form: `CODE reason [field]: message`. `chartroom apply` exits 3 for `E_PARSE`, 4 for `E_NEEDS_SUBTOOL`, 2 for a usage error and 1 if it cannot write.
