# n8n: owner instance access and scoped agent flow tools

The owner `n8n` entry fronts the instance MCP with original tool names, schemas,
metadata, results and errors. Agents use a separate `n8n-flows` entry backed by an
MCP Server Trigger workflow. Its tools map to the `flows.run` operation and require
reviewed logical flow bindings. Neither catalog synthesizes operations from newly
discovered tools; the instance catalog has no agent operations.

This implementation uses stock Community features and the existing broker
Streamable HTTP adapter. It adds no workflow CRUD client, n8n API credential,
Enterprise code, or PERSIST owner token. The
[MCP Server Trigger documentation](https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-langchain.mcptrigger)
describes workflow tools and Streamable HTTP support. A PERSIST flow-run HTTP
endpoint is not assumed: this path works with the broker's existing transport and
keeps management authority on the owner connector. PERSIST still mints grants,
tracks runs, deploys reviewed flows and reconciles execution receipts.

## Owner installation — Apex applies to the live broker

Instance contract read on 2026-11-04 UTC:
`~/Projects/skill-workspaces/agent-fleet/2026-11-04/n8n/INSTANCE.md`.
The instance endpoint is **http://127.0.0.1:5678/mcp-server/http** using
**Streamable HTTP**, authenticated with a bearer token from Keychain service
`n8n-mcp-token`, account `persist`. The public API at `/api/v1` uses a different
credential and is not needed by this connector.

After integrating the code, securely import the existing Keychain value through
the broker's existing stdin secret mechanism. This duplicates it only into the
owner-only broker secret store, not the connector YAML, git or command arguments:

```sh
set -o pipefail
security find-generic-password -s n8n-mcp-token -a persist -w |
  brokerd secret set N8N_MCP_TOKEN
```

Run without shell tracing or terminal recording of pipe contents. The secret
store is `~/.brokerd/secrets.json` (0600). Missing values leave the connector
unloaded. Rotation repeats the import; do not put the actual token in YAML.

Merge this exact entry under the existing `connectors:` map (also available in
[the tested example](examples/n8n-connectors.yaml)):

```yaml
connectors:
  n8n:
    url: http://127.0.0.1:5678/mcp-server/http
    headers:
      Authorization: "Bearer ${secret:N8N_MCP_TOKEN}"
    owner_only: true
    expose: passthrough
    clients: [claude-code, codex]
```

Do not add `tools`, `strip`, `response` or a custom catalog to this entry: the
owner receives the instance as-is. `owner_only` is enforced at grant minting,
discovery and dispatch, including pre-existing grants. The embedded empty `n8n`
catalog also prevents agent access if the flag is accidentally removed.

Configuration and secrets hot-reload; this task does not authorize a live binary
installation or restart. Once Apex has integrated/activated the new build:

```sh
brokerd status
brokerd client install claude-code --mode servers
brokerd client install codex --mode servers
```

These commands retain all existing connectors and create a separate `n8n` server
entry pointing to `http://127.0.0.1:8890/mcp/n8n` with the client's broker token.
The owner n8n bearer stays inside brokerd. Preserve existing per-server client
permission rules; refresh the clients' MCP connections after installation.
Use `--mode servers` to retain original tool names without an aggregate prefix.

## Agent setup — fixture workflow deployment required

1. Keep artifacts in
   `<project>/.persist/flows/<flow-name>/{workflow.json,manifest.yaml,fixtures/*.json,README.md}`.
   The manifest carries logical ID, owner, input schema, credential aliases,
   effect class (`read|write|send|spend`) and idempotency key path.
2. Have the trusted deployer review nodes, destinations, credential aliases,
   input schemas and effects; publish only a fixture workflow initially. Attach
   reviewed Custom n8n Workflow Tools to an MCP Server Trigger. Copy that node's
   **Streamable HTTP production URL**, not the instance MCP URL. A published
   fixture route is still fixture-only; no production provider effects are
   authorized here.
3. Use a dedicated trigger bearer stored as Keychain service
   `n8n-flow-mcp-token`, account `persist`, then import it as `N8N_FLOW_MCP_TOKEN`
   using the same pipe pattern. This is a separate narrow credential, never the
   instance MCP/API key. Provisioning the trigger credential/workflow belongs to
   the trusted n8n deployer, not agent grants.
4. Copy [the catalog template](examples/n8n-flows.yaml) to
   `~/.brokerd/catalog.d/n8n-flows.yaml`, replacing tool and deployment fields
   with observed values. Bind each tool to a distinct logical ID; repeat entries
   with `id: flows.run` for additional tools. Use the strongest effect class the
   reviewed flow can perform; flows with multiple classes must be split or kept
   unavailable until all their authority requirements are represented.
5. Merge this agent connector after replacing the path with the trigger's actual
   URL. No endpoint/path or flow binding is installed automatically:

```yaml
connectors:
  n8n-flows:
    url: http://127.0.0.1:5678/mcp/REPLACE_WITH_TRIGGER_PATH
    headers:
      Authorization: "Bearer ${secret:N8N_FLOW_MCP_TOKEN}"
    call_timeout: 120s
```

The embedded `n8n-flows` catalog is empty and denies all agent tools until an
owner installs reviewed bindings. Newly exposed upstream tools remain hidden and
unresolvable. Never point this connector to `/mcp-server/http`. Management tools
have no binding; agents cannot create/update/delete/publish workflows. Editing a
workflow can change its effects, so deployer access and catalog updates stay
owner-controlled. A logical flow ID or a folder is not n8n access control.

## Grant contract

Example broker capabilities (PERSIST must pass these into its normal broker
mint request, with a real `run_id`; configuring PERSIST agent YAML is a separate
integration step):

```json
{
  "n8n-flows": {
    "operations": ["flows.run"],
    "constraints": {"flow_ids": ["fixture.echo"]},
    "limits": {"max_per_day": 5}
  }
}
```

All flow tools use operation **`flows.run`**. A read flow needs `flows.run` plus
its allowlisted ID. Other flows additionally require `flows.write`, `flows.send`
or `flows.spend`, respectively, in `operations`. Caller-supplied effect labels
cannot change the catalog's binding. Each inherited connector and operation
`flow_ids` allowlist must admit the ID; missing, empty or malformed lists deny.
Operation constraints may narrow the shared `flows.run` limits/allowlist.

`max_per_day` defaults to **0** (deny). A positive grant ceiling is required.
The existing atomic SQLite counter is shared by agent/project/`flows.run`/UTC day
across all tools and grants; a new grant or broker restart does not reset it.
The budget counts admitted attempts, including provider failures or failed
intent writes, and is not a monetary amount. `flows.spend` authorizes only the
reviewed flow; bound amounts/recipients/targets in the reviewed flow and add
catalog argument constraints before any real spend/send/write use. This task
qualifies fixtures only.

## Workflow input/output and correlation

Workflow tool input schemas must accept the top-level fields `persist_run_id`,
`logical_flow_id`, `git_revision`, and (for effects) `idempotency_key`, alongside
reviewed business input fields. Leave the injected correlation fields optional
in the public input schema so MCP validation can run before policy injects them.
Broker policy sets the three correlation fields
from the grant/catalog even if the caller supplies conflicting values. Bind
`git_revision`, `workflow_id` and `workflow_version` from the reviewed deployed
artifact in the catalog; full 40- or 64-digit git hashes are required.

An effectful flow needs a non-empty top-level `idempotency_key`. Map it to the
manifest's key path and enforce deduplication at the effect boundary. The broker
records the key but does **not** guarantee exactly-once execution or implement
workflow-side deduplication. Keep user input away from dynamic destinations,
workflow selection, credentials and expression/code execution.

Return a JSON object (MCP structuredContent or a single JSON text block):

```json
{
  "persist_run_id": "<grant run_id>",
  "logical_flow_id": "fixture.echo",
  "git_revision": "<reviewed full git revision>",
  "n8n_workflow_id": "<actual workflow id>",
  "n8n_workflow_version": "<actual deployed version>",
  "n8n_execution_id": "<actual execution id, string>",
  "data": {"fixture": true}
}
```

The workflow must derive workflow/version/execution fields from its actual n8n
execution context, not echo caller labels. Broker validation requires matching
correlation and deployment metadata plus a non-empty execution ID. Missing or
mismatched fields, timeout, or a connection lost after dispatch produce an
`outcome_unknown` receipt: reconcile, never blindly retry effects. This checks the
trusted workflow's report, not an independent n8n execution API readback.
It is also not a pre-dispatch version fence: the deployer must disable/review and
rebind a tool before changing its workflow, credentials or destinations. A drift
detected in returned metadata can be detected only after that run has executed.
PERSIST must retain the returned workflow ID/version/execution ID alongside its
run; that PERSIST ledger integration is not implemented in this Go worktree.

The normal broker path writes intent before dispatch and an outcome or denial
receipt afterward. Receipts include grant/run/project, `flows.run`, effective
argument digest, policy/catalog version and execution ID (`upstream_request_id`).
Input summaries are truncated and may contain business data: do not send secrets
or sensitive provider payloads as workflow inputs. PERSIST reconciliation should
read actual n8n execution records through its trusted deployer boundary. Owner
instance calls continue to receive raw upstream responses without these agent
checks or added correlation fields.

## Verification and rollback

Local tests use synthetic MCP servers, real HTTP transports, scratch SQLite and
fixture tokens. They cover owner passthrough (including error/structured/meta
content), management denial, allowlists, inherited grants, effect gates,
idempotency requirements, required budgets, cross-grant quotas, correlation and
unknown outcomes. No live broker/client config, n8n workflow or credential was
changed by the builder.

Apex's live acceptance after integration:

- Check `brokerd status`, owner `n8n` initialize/tools/list versus the instance's
  own discovery; compare names, schemas and metadata. Use only an explicitly
  reviewed read-only tool; management mutation is not needed for acceptance.
- Mint an agent fixture grant naming `n8n-flows`; assert only its allowed read
  tool appears. Confirm instance grant minting is refused and management tool
  guesses cannot dispatch through the flow endpoint or aggregate endpoint.
- Run the fixture with a PERSIST run ID and reconcile its actual workflow ID,
  version and execution ID with intent/outcome receipts and the PERSIST ledger.
- Try a foreign logical ID, missing effect capability and exhausted daily
  budget; verify denials and no additional n8n executions. Exercise no real
  provider writes, sends or spend.

Disable the two connectors (or remove only their entries) and let config
hot-reload; revoke fixture grants. Remove the flow catalog and unused broker
secret keys only after connector removal. Restore prior client entries with
`brokerd client restore n8n` when an adopted entry existed. Do not use a global
client revert that would affect unrelated servers. No database migration is
needed. Keep existing receipts and daily counters during rollback.
