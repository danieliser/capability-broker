# Operation constraints and broker acceptance

The embedded catalog currently contains 17 operations. There are no embedded mail, payment, publish, or spend
operations. The declarative rules below are available to operator-authored catalog overlays; adding a rule does
not add a connector, expose a new operation, or authorize sending/spending.

## Call order

The front authenticates the current grant, resolves the operation, and evaluates capability requirements and
legacy checks. The upstream adapter then prepares arguments (named SQL and bound parameters, forced draft status,
namespace/scoped recall hooks). A **final declarative constraint pass** runs on those effective arguments. Only
then does the front write an intent and execute the action. The intent and outcome digest the prepared arguments.
Preparation may perform read-only ownership lookups; it must not execute the requested action. A custom adapter
can implement `adapter.Preparer`; execution must use the returned arguments without widening them.

Owner clients retain their existing passthrough behavior and bypass the new argument restrictions. Agent grants
fail closed on unknown rule kinds, malformed configured values, missing required keys, null/non-object path
parents, or counter-store errors. Lists and nested request objects are copied before normalization.

## Every embedded operation

All 17 operations retain `calls_per_run` and now declare `max_per_day` (enforced when configured). The daily cap
is per agent principal + project + connector + operation, across grants, sessions and daemon restarts. Days are
UTC. It counts admitted attempts, including later provider/audit failures, rather than claiming successful sends.
Denied pure argument checks do not consume the daily budget. Legacy per-run counters remain conservative and may
consume an attempt before a later argument check fails.

| Operation | Required authority and argument checks | Enrichment / result controls |
|---|---|---|
| `clickhouse.named_query` | Named query in grant and catalog; row/time/run/day bounds | Resolve SQL and declared bound params; minimum query/grant/default row ceiling; result ceiling and call deadline |
| `clickhouse.sql.select` | `sql.select`; row/time/run/day bounds | Single read statement; no SETTINGS/OUTFILE; result ceiling and call deadline |
| `clickhouse.schema.databases` | `sql.select`; row/time/run/day bounds | Result ceiling and call deadline |
| `clickhouse.schema.tables` | `sql.select`; row/time/run/day bounds | Result ceiling and call deadline |
| `memory.store` | `store`; namespace; store/run/day bounds; importance ceiling | Single memory only; force own namespace tag; reject batch/supersede |
| `memory.recall` | `recall`; namespace; run/day bounds; `limit` ≤ `max_rows` | Default limit 50 when no cap; exact scoped recall; filter foreign namespace results; result ceiling also applies to verbose |
| `memory.associate` | `store`; namespace; run/day bounds | Ownership lookup for both IDs; reject batch associations |
| `memory.update` | `update`; namespace; importance ceiling; run/day bounds | Ownership lookup; supplied tags cannot escape scope and retain own namespace |
| `memory.delete` | Explicit `delete`; namespace; run/day bounds | Ownership lookup; no bulk-by-tag for agents |
| `memory.health` | Connector access; run/day bounds | No memory arguments |
| `wordpress.content.read` | `content.read`; allowed site; run/day bounds; optional `arguments.per_page` ≤ configured `max_rows` | Default sole site; disable persisted block refs; compact request; configured result ceiling |
| `wordpress.content.draft` | `content.draft`; site/post type; draft/run/day bounds | Supported create abilities only; force `status=draft`; compact request |
| `wordpress.run_across` | Explicit `run_across`; nonempty allowed sites; run/day bounds | Omitted sites narrowed to grant sites; empty/foreign sites refused |
| `wordpress.site.read` | `content.read`; site filter; run/day bounds | Supply grant sites; refuse empty/foreign filters |
| `wordpress.abilities.search` | `content.read`; site filter; run/day bounds | Supply grant sites; refuse empty/foreign filters |
| `wordpress.abilities.describe` | `content.read`; allowed site; run/day bounds | Default sole site |
| `wordpress.content.read_url` | `content.read`; URL hostname maps to a granted site; run/day bounds | Site result unwrapping |

WordPress `run_across` is an explicit broad capability: it can run writes as well as reads on the granted sites.
Its existing capability does not inherit `content.draft`'s create-only/post-type restrictions. Do not grant it
when a read/draft grant is intended. Unmatched `wp_run` abilities remain denied.

## Declarative overlay rules

`argument_constraints` is a list on an operation. `path` is a dot-separated object path, with literal keys and
no wildcards/indexes. `key` looks up a value in the grant's connector `constraints`, connector `limits`, and
`operation_constraints[full operation ID]`. Every supplied restriction applies: operation values can narrow
connector values. A numeric `default` is used only when no grant value is supplied.

| Kind | Behavior |
|---|---|
| `allowlist` | A string or nonempty list of strings; **every** item must exactly match the allowlist. Case-sensitive; no implicit address parsing, splitting or patterns. |
| `prefix` | String must start with the configured nonempty prefix, case-sensitive. |
| `maximum` | Finite, nonnegative numeric argument ≤ ceiling; strings, NaN/infinity and values outside JSON's safe integer range are refused. Units/currency must be defined by the operation. |
| `row_limit` | Positive integer argument ≤ integer ceiling; refuses over-limit requests. A missing argument is supplied only with `required: true` or a `default`. Also bounds collection results. |
| `max_per_day` | Nonnegative integer ceiling; zero denies all attempts. Optional without a default means no daily cap unless supplied. Has no path. |

`required: true` requires a configured grant key and an argument, except row limits can supply their bound as the
argument. Optional missing arguments are untouched (so a single-record WordPress ability gains no pagination
argument). Missing intermediate objects are created only for a row default/required injection.

For a hypothetical send operation (example only, no such operation is enabled):

```yaml
argument_constraints:
  - {kind: allowlist, path: to, key: recipients, required: true}
  - {kind: allowlist, path: cc, key: recipients}
  - {kind: allowlist, path: bcc, key: recipients}
  - {kind: prefix, path: subject, key: subject_prefix, required: true}
  - {kind: max_per_day, key: max_per_day}
```

For a hypothetical payment operation, use a required `maximum` on `amount` with key `max_amount`, a required
currency `allowlist`, and the separate approval mechanism. A ceiling alone never grants approval.

```json
{
  "example": {
    "operations": ["send"],
    "constraints": {"recipients": ["ops@example.test"], "subject_prefix": "[Fleet] "},
    "limits": {"max_per_day": 5},
    "operation_constraints": {
      "example.send": {"subject_prefix": "[Fleet] Daily ", "max_per_day": 2}
    }
  }
}
```

Child grants retain parent connector and operation restrictions. Allowlists intersect, numeric ceilings take the
minimum, compatible prefixes take the narrower prefix, and incompatible/malformed inherited values fail closed.
Legacy specialized checks (site/namespace/post type/importance/per-run counters) use connector fields; the new
operation map applies to declarative rules. Unknown custom constraint values are not an authorization feature.

Row ceilings refuse oversized recognized collection payloads, including verbose results, rather than silently
truncating evidence. They cover root arrays and the `rows`, `data`, `results`, `items`, `posts`, `sites`, `abilities`,
`databases`, `tables` collections, including `result` envelopes. This bounds returned rows, **not provider scan
cost, server-side execution, or spend**. ClickHouse's read-only user profile must retain provider-side settings.
The stock `run_query` schema accepts only query/params, so no unsupported settings arguments are injected.
Broker call deadlines use the smaller of the connector timeout and effective execution limit; provider cancellation
is best effort. New provider result envelopes need explicit qualification before claiming coverage.

## Isolated steward acceptance

From this worktree (Go 1.26.5):

```sh
mkdir -p output/tmp
TMPDIR="$PWD/output/tmp" go test ./test/e2e -run '^TestStewardBrokerAcceptance$' -v -count=1
```

The test creates a loopback broker instance, local MCP provider fixtures, and a private SQLite database. It uses
the real broker front and upstream adapter for a steward grant: named query read → store the read result through
`/mcp/memory` → recall through that front. It checks forced namespace/importance, bound query params, foreign-record
filtering, denial of raw SQL and foreign tags, effective argument digests, memory ID receipt, and all eight persisted
receipts (three intent/outcome pairs, two denials). A SQL-prefix constraint that only the resolved SQL satisfies
locks in post-enrichment ordering. It does not boot the installed daemon, call ClickHouse/AutoMem live, or prove a
PERSIST-scheduled wake. Temporary test files stay under ignored `output/` with the command above.

Apex owns install/live qualification. After installing the reviewed binary, use a newly minted test grant with a
private test namespace and known named query; perform the same read/store/recall through the broker URLs and pull
receipts with `brokerd receipts`. Confirm the actual named-query result, memory ID/tags and paired receipt identity,
policy version/digests. Record any test memory IDs for separate owner-approved cleanup. Do not reuse the live smoke
tag or infer live provider proof from this fixture test.

The built cleanup command is also exercised through a local broker by `TestCleanupCommandDefaultsToDryRun`; inventory and manifest defaults perform no writes, while explicit execute deletes only a local fixture.

## Existing smoke residue: read-only inventory and cleanup command

The exact inventory and API page receipt are in [evidence/2026-11-03-broker-smoke.json](evidence/2026-11-03-broker-smoke.json)
and [evidence/2026-11-03-broker-smoke-api.json](evidence/2026-11-03-broker-smoke-api.json). On 2026-11-03 at 10:27:03 UTC,
exact `broker-smoke-test` enumeration returned one record and `has_more: false`:

- `671c092f-9496-483a-a60e-4a3d3c792053`, tags `broker-smoke-test`, `agent:companykit.data-sync-steward`.

No live memory was deleted. The API is exact, case-insensitive, **any-of**, so namespace intersection happens
locally; passing both tags to enumeration would widen the selection. Enumeration accepts only tags, exhaustive,
limit, offset, tag_mode=any, tag_match=exact and format. In particular, do not add `current_only` to this mode.

The cleanup CLI uses `BROKER_TOKEN` from the environment (an existing owner token supplied securely by Apex; no
credentials in argv/output) and connects only to a loopback `/mcp/memory` endpoint. Inventory mode and manifest
validation are read-only by default:

```sh
mkdir -p output/tmp
TMPDIR="$PWD/output/tmp" go build -o output/brokerd-memory-cleanup ./cmd/brokerd-memory-cleanup
# BROKER_TOKEN is already set securely; do not echo it.
output/brokerd-memory-cleanup > output/smoke-current.json
output/brokerd-memory-cleanup -manifest docs/evidence/2026-11-03-broker-smoke.json
```

Only Apex/Daniel, after reviewing the exact IDs and obtaining deletion authority, runs:

```sh
output/brokerd-memory-cleanup -manifest docs/evidence/2026-11-03-broker-smoke.json -execute
```

Execution re-fetches and validates **all** manifest IDs before any delete, checks both exact tags and unchanged
full-content SHA-256, deletes by ID only, and re-enumerates to confirm reviewed IDs are absent. A changed record,
partial inventory page, duplicate ID or missing scope aborts. A provider failure mid-batch can leave a partial
cleanup: re-inventory and review before retrying. AutoMem has no conditional-delete transaction, so this cannot
eliminate a concurrent edit between validation and deletion. The manifest preserves original content/tags for
manual recovery; recreation would receive a new ID and would not restore graph relationships. There is no automatic
undo. Do not use bulk tag deletion.

## Schema / rollback

`daily_counters` is an additive, idempotent SQLite table created on first open. No existing rows are rewritten.
The old binary ignores it; rollback leaves it in place. Existing grants without `max_per_day` continue without a
daily cap. Operator `catalog.d` overlays replace the embedded connector entirely: Apex must review/port the new
rules into any active overlay to enable them there. The builder makes no live config/database edits or restarts.

### Independent review corrections

Owner grants bypass agent argument policy and request guards. Connector exposure
and explicitly configured response shaping still apply; receipts still record the
arguments actually forwarded. An exposed owner tool action need not match an agent
operation pattern. Empty inherited prefixes remain denied through child grants.
Cleanup inventory requests exact tag matching and revalidation compares scope tags
byte for byte, without Unicode or case folding.
