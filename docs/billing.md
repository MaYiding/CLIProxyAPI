# Built-in API Key Billing

CLIProxyAPI can write an append-only, request-level billing ledger and calculate costs from configurable per-million-token prices. Metering happens at the shared usage layer, so the same accounting path covers OpenAI-compatible, Codex, Claude, Gemini, and plugin executors that publish standard usage records.

## What is recorded

Each JSONL event contains:

- the client API key SHA-256 identifier, mask, and optional display name;
- provider, executor, upstream model, requested alias, endpoint, and request ID;
- success/failure status, latency, time to first token, credential index, and service tier;
- input, output, reasoning, cached, cache-read, cache-creation, and total tokens;
- normalized billable input/output tokens;
- the matched price rule, frozen unit prices, counter-overlap modes, and a component cost breakdown.

Raw client API keys are never persisted. API-key-based upstream sources are also hashed and masked before they are written. The ledger can still contain operational metadata such as OAuth account email addresses, model names, and endpoints, so protect it like other service accounting data.

## Configuration

```yaml
billing:
  enabled: true
  # Empty means <auth-dir>/billing.jsonl.
  # A relative path is resolved from the config file directory.
  store-path: ""
  currency: "USD"
  # Optional: fsync every event for stronger power-loss durability.
  sync-on-write: false

  # Fallback for every token category when no rule below matches.
  # Omitted defaults to 1.00; explicit 0 is allowed.
  default-price-per-million: 1.00

  # Display names used by the dashboard and reports.
  # The configuration key keeps its legacy name for compatibility.
  key-labels:
    "client-api-key-a": "customer-a"
    "client-api-key-b": "internal-tools"

  # Cumulative spend limits in the configured currency. Missing/0 is unlimited.
  key-limits:
    "client-api-key-a": 25.00
    "client-api-key-b": 0

  prices:
    - name: "gpt-production"
      provider: "openai"
      model: "gpt-*"
      input-per-million: 1.00
      output-per-million: 4.00
      reasoning-per-million: 4.00
      cache-read-per-million: 0.10
      cache-creation-per-million: 1.00
      input-cache-mode: "auto"
      reasoning-mode: "auto"

    - name: "fallback"
      provider: "*"
      model: "*"
      input-per-million: 0.50
      output-per-million: 1.50
```

Advanced prices in this example are illustrative, not current vendor prices. CLIProxyAPI does not fetch provider prices. The built-in fallback is one currency unit per million tokens, as configured by `default-price-per-million`; update it and any advanced rules from your own contract or price source.

Rules are evaluated in order, and the first match wins. `provider` and `model` are case-insensitive glob patterns supporting `*` and `?`. Requests without a matching advanced rule use `default-price-per-million` for input, output, reasoning, cache-read, and cache-creation tokens after overlap normalization, so each normalized token is charged once. Historical events keep the price snapshot that was active when they were recorded.

If an upstream reports `total_tokens` but omits part or all of the category breakdown, the positive unclassified remainder is added to `billable_input_tokens` and charged at the matched input rate. This prevents total-only usage records from silently becoming free while keeping the original provider counters visible.

`key-limits` is keyed by the same downstream client keys configured under top-level `api-keys`. Limits are cumulative across the active ledger. When a key's persisted spend is equal to or greater than its positive limit, all authenticated proxy routes reject new requests with HTTP `429`, error code `billing_limit_exceeded`, and `X-Billing-*` amount headers. Raising the limit immediately unblocks the key; `0` or a missing entry is unlimited. Requests already in flight when another request crosses a limit can finish and be billed, so a small concurrent overshoot is possible by design.

`usage-statistics-enabled` controls the short-lived usage queue and is independent of billing. Billing only requires `billing.enabled: true`.

## Detailed token accounting

Providers do not all describe detailed counters the same way. For example, cached input can be a subset of `input_tokens` or an additional counter, and reasoning can be a subset of `output_tokens` or an additional counter.

The default `auto` mode evaluates the possible included/additional combinations and selects the one whose component sum best matches the reported `total_tokens`. The resolved modes and normalized billable input/output counters are frozen into every event.

For a provider with a known contract, set either field explicitly:

- `input-cache-mode: included` subtracts cache-read and cache-creation tokens from billable input before applying the normal input price.
- `input-cache-mode: additional` leaves normal input unchanged and prices cache counters separately.
- `reasoning-mode: included` subtracts reasoning tokens from billable output before pricing reasoning separately.
- `reasoning-mode: additional` leaves normal output unchanged and adds reasoning cost separately.

Costs are stored as integer billionths (`*_nanos`) of the configured currency unit. The `total` field is a nine-decimal string, which avoids cumulative floating-point drift in reports.

## Management API

The authenticated endpoint is:

```text
GET /v0/management/billing/usage
```

Supported query parameters:

| Parameter | Meaning |
| --- | --- |
| `from` | Inclusive RFC3339 timestamp or Unix seconds |
| `to` | Inclusive RFC3339 timestamp or Unix seconds |
| `key_id` | Exact SHA-256 client key identifier returned by the API |
| `provider` | Exact provider filter |
| `model` | Exact upstream model filter |
| `limit` | Event page size, default 100, maximum 1000 |
| `offset` | Event page offset, default 0 |

Example:

```bash
curl -H 'Authorization: Bearer <management-key>' \
  'http://127.0.0.1:8317/v0/management/billing/usage?from=2026-07-01T00:00:00Z&provider=openai&limit=100'
```

The response includes totals across every matching event, `by_key` and `by_model` aggregates, and a reverse-chronological paginated `events` list. Pagination affects only `events`, not the aggregates.

Editable settings are available through:

```text
GET /v0/management/billing/settings
PUT /v0/management/billing/settings
POST /v0/management/billing/keys/:key_id/reveal
```

These endpoints power the built-in dashboard. The settings response includes a stable hashed ID plus a real display preview: short keys are shown in full, while long keys use the first eight and last four characters. The reveal endpoint returns exactly one complete, currently active client key by its hashed ID, uses the existing management authentication, and sends non-cacheable responses. The full key is not placed in a URL, log, report, or ledger. The PUT endpoint accepts complete billing settings, including per-key display names and limits, validates them, persists `config.yaml`, and applies the new configuration immediately.

## Web dashboard

Open the management center at:

```text
http://127.0.0.1:8317/management.html
```

Sign in once with the normal management-center credentials, then select **Billing & usage** in the left sidebar. No second authentication step is required. The default **KEY management** tab puts each display name, key preview/reveal control, cumulative spend, limit, remaining amount, and live stop status together. Separate tabs contain usage filters/request details and global/default/advanced price settings. The layout is localized with the management center and adapts to mobile screens without forcing the core key-management table into a wide horizontal scroll. The legacy `/billing.html` URL redirects to this integrated page. The module is embedded in the server binary, so it remains available independently of management-center asset updates.

The dashboard follows the same availability rules as `management.html`: it returns `404` when the control panel is disabled or the process runs in Home mode. API authentication and filtering remain enforced by `/v0/management/billing/usage`.

## Operational notes

- The ledger is loaded on startup so management queries can aggregate historical events. Use an external usage service for very large or multi-node deployments.
- `sync-on-write: true` fsyncs every event for stronger power-loss durability. The default `false` writes each complete JSONL record immediately and fsyncs during reconfiguration or graceful shutdown.
- Use one CLIProxyAPI process per ledger file. The JSONL store does not coordinate concurrent writers across processes.
- Retries and additional-model calls are separate upstream usage events. This reflects actual provider-side work and avoids undercounting failed-over requests.
- Failed requests with reported token usage are priced normally. Failed requests without usage metadata remain visible with zero tokens and zero cost.
- Unit prices are frozen into each event, so changing configuration affects new requests without rewriting historical charges.
- Key limits use cumulative frozen event costs from the active ledger. Use a new ledger path when starting a completely new accounting period.
- A ledger cannot mix currencies. To change `currency`, select a new `store-path`; startup rejects a ledger whose recorded currency differs from the configured currency.
- The built-in API intentionally has no destructive ledger endpoint. Archive or rotate the file with your normal retention process.
