# The contract Chartroom implements

`SPEC.md` says catalog entries follow "a single deterministic schema enforced by Muster" and does not give the schema. Muster does not exist yet. This page is the schema Chartroom was built against, written down so it can be reviewed, corrected and moved into the specification.

Everything here is contract version `v1`. All data in the examples is made up.

## An item

An item is a decision that has already been made upstream. Chartroom publishes it; it does not judge it.

```json
{
  "contract_version": "v1",
  "item_id": "itm-0101",
  "state": "building",
  "action": "publish",
  "slug": "sample-clause-finder",
  "entry": { "title": "Sample Clause Finder", "summary": "One line about the tool." },
  "publish": { "repository": "https://forge.example/tools/sample-clause-finder" },
  "provenance": { "note": "Stamped upstream, published byte for byte.", "links": ["https://rules.example/part-900/section-12"] },
  "attestation": { "conditions": [ { "name": "tests-pass", "passed": true } ] }
}
```

| Field | Rule |
| --- | --- |
| `contract_version` | Must be `v1`. Anything else is `E_NEEDS_SUBTOOL`. |
| `item_id`, `slug` | Lower-case letters and digits in groups joined by single hyphens, at most 64 bytes. |
| `state` | The item's current state. Must be the source state of the action (table below). |
| `action` | `publish`, `link`, `merge` or `retire`. Anything else is `E_NEEDS_SUBTOOL`. |
| `entry.title` | 1 to 120 characters, one line, no control characters, no white space at the ends. |
| `entry.summary` | 1 to 600 characters, same rules. |
| `provenance.note` | Not blank, at most 4000 bytes. Published exactly as received. |
| `provenance.links` | 1 to 16 public links. |
| `attestation.conditions` | Exactly five, each with its own name, every one `passed: true`. |

The whole item is at most 64 KiB. It is read strictly: invalid UTF-8, a duplicate key, a field not listed here, a key in another letter case and trailing data are all `E_PARSE`. The provenance block has exactly two fields, so a raw snapshot cannot be carried through it.

A **public link** is an absolute `https` URL with no user name or password, whose host is a lower-case dotted DNS name: not an IP address, not a single label, and not ending in `.local`, `.internal`, `.localhost` or `.svc`.

### What each action carries

| Action | From → to | Must carry | Must not carry |
| --- | --- | --- | --- |
| `publish` | `building` → `live` | `entry`, `publish.repository` (public link), `provenance`, `attestation` | `link`, `merge`, `retire` |
| `link` | `scored` → `linked` | `entry`, `link.name`, `link.url` (public link), `provenance` | `publish`, `merge`, `retire`, `attestation` |
| `merge` | `scored` → `merged` | `entry`, `merge.parent` (another slug), `merge.note` | `publish`, `link`, `retire`, `attestation` |
| `retire` | `live` → `retired` | `retire.reason` (`repealed` or `superseded`), `retire.note` | `entry`, `publish`, `link`, `merge`, `provenance`, `attestation` |

A `merge` may carry `provenance`; if it does, the block is checked and published. Notes (`merge.note`, `retire.note`) are 1 to 2000 characters and may hold line breaks.

## The catalog

```json
{
  "contract_version": "v1",
  "entries": [
    {
      "slug": "sample-clause-finder",
      "status": "live",
      "title": "Sample Clause Finder",
      "summary": "One line about the tool.",
      "repository": "https://forge.example/tools/sample-clause-finder",
      "provenance": { "note": "…", "links": ["https://rules.example/part-900/section-12"] }
    }
  ]
}
```

`status` is `live`, `linked`, `merged` or `retired`. A `linked` entry has `link` in place of `repository`. A `merged` entry has `merge` and neither. A `retired` entry is the live entry as it was, with `status` changed and a `retirement` block (`reason`, `note`) added: that block is the historical note.

Chartroom always writes the catalog the same way: entries in slug order, two-space indent, no HTML escaping, one trailing newline. An entry carries nothing from the item except the fields above: no attestation, no item identifier, no state.

## What an action does to the catalog

| Action | Catalog before | Result |
| --- | --- | --- |
| `publish`, `link`, `merge` | No entry with this slug | The entry is added. |
| `merge` | Parent slug is not a `live` entry | `E_NEEDS_SUBTOOL` |
| `retire` | Entry is `live` | The entry becomes `retired` and gains the retirement note. |
| any | The entry is already exactly what this item would make it | No change (`"changed": false`). A retry is harmless. |
| any | Anything else | `E_NEEDS_SUBTOOL`. There is no rule for it. |

## The change

What `chartroom apply` prints, and what the worker hands to Muster in `complete`:

```json
{
  "contract_version": "v1",
  "item_id": "itm-0101",
  "action": "publish",
  "from": "building",
  "to": "live",
  "slug": "sample-clause-finder",
  "changed": true,
  "before": null,
  "after": { "slug": "sample-clause-finder", "status": "live", "…": "…" }
}
```

## Errors

Three codes, each with a fixed list of reasons. Every message is a constant; nothing from the data, the network or the operating system is copied into one. `field` is the name of a contract field, never a value.

| Code | Reason | When |
| --- | --- | --- |
| `E_PARSE` | `malformed_json` | Not well-formed JSON, not an object, wrong type, duplicate key, invalid UTF-8, trailing data, too large. |
| `E_PARSE` | `unknown_field` | A field that is not in the contract. |
| `E_PARSE` | `invalid_metadata` | A field is missing or breaks its rule, or a block is missing or belongs to another action. |
| `E_PARSE` | `missing_provenance` | The provenance block is absent, or has a blank note or no links. |
| `E_PARSE` | `missing_attestation` | The attestation is absent or does not show five distinct passed conditions. |
| `E_PARSE` | `invalid_catalog` | The catalog itself cannot be read or an entry in it has the wrong shape. |
| `E_NEEDS_SUBTOOL` | `unsupported_entry_type` | An action, or a catalog entry status, that this build does not know. |
| `E_NEEDS_SUBTOOL` | `schema_version_mismatch` | A contract version other than `v1`, in the item, the catalog or `CONTRACT_VERSION`. |
| `E_NEEDS_SUBTOOL` | `unsupported_transition` | The item's state is not the source state of its action. |
| `E_NEEDS_SUBTOOL` | `unsupported_catalog_state` | The catalog is in a state the action has no rule for. |
| `E_NEEDS_SUBTOOL` | `unsupported_value` | A retirement reason or a priority that this build does not know. |
| `E_TIMEOUT` | `upstream_timeout` | Muster did not acknowledge a call. |

Printed form: `CODE reason [field]: message`. `chartroom apply` exits 3 for `E_PARSE`, 4 for `E_NEEDS_SUBTOOL`, 2 for a usage error and 1 if it cannot write.

## Talking to Muster

Five calls, each an HTTP `POST` with a JSON body and `Authorization: Bearer <ServiceAccount token>`. The paths and body shapes are provisional: Muster's own specification will fix them.

| Call | Path | Body |
| --- | --- | --- |
| lease | `/v1/lease` | `{"worker": "chartroom", "contract_version": "v1", "max": 4}` |
| heartbeat | `/v1/heartbeat` | `{"lease_id": "…"}` |
| complete | `/v1/complete` | `{"lease_id": "…", "change": { … }}` |
| fail | `/v1/fail` | `{"lease_id": "…", "code": "E_PARSE", "reason": "…", "field": "…", "message": "…"}` |
| needs_subtool | `/v1/needs_subtool` | the same, with `E_NEEDS_SUBTOOL` |

A lease answer is `{"leases": [{"lease_id", "priority", "item", "catalog"}]}`. `priority` is `urgent`, `normal` or `backfill`. `catalog` is the part of the catalog the item can touch: at least the entry with the same slug and, for a merge, the parent.

How Chartroom reads an answer:

| Answer | Meaning |
| --- | --- |
| `200`, `204` | Acknowledged. |
| `404`, `409`, `410` to anything but lease | Muster no longer holds the lease. The staged result is dropped; Muster leases the item again if it needs to. |
| Anything else, a timeout, no connection | Not acknowledged. The result stays staged and is sent again after 1 s, 2 s, 4 s and so on, up to 60 s. |

Chartroom never follows a redirect and never uses a proxy.
