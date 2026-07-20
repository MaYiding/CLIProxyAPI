# Built-in API Key Billing

CLIProxyAPI can write an append-only, request-level billing ledger and calculate costs from configurable per-million-token prices. Metering happens at the shared usage layer, so the same accounting path covers OpenAI-compatible, Codex, Claude, Gemini, and plugin executors that publish standard usage records.

## What is recorded

Each JSONL event contains:

- the client API key SHA-256 identifier, mask, and optional label;
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

  key-labels:
    "client-api-key-a": "customer-a"
    "client-api-key-b": "internal-tools"

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

Prices in this example are illustrative, not current vendor prices. CLIProxyAPI does not fetch or assume provider prices. Update the rules from your own contract or price source.

Rules are evaluated in order, and the first match wins. `provider` and `model` are case-insensitive glob patterns supporting `*` and `?`. Requests without a matching rule are still fully recorded and reported as `unpriced_requests`; their cost is zero instead of an invented estimate.

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

## Web dashboard

Open the built-in dashboard at:

```text
http://127.0.0.1:8317/billing.html
```

Enter the management key when prompted. The key is held only in the current browser tab and is not written to local storage. The dashboard provides aggregate cards, per-key and per-model tables, request details, filters, and pagination. It is embedded in the server binary, so it remains available independently of management-center asset updates.

The dashboard follows the same availability rules as `management.html`: it returns `404` when the control panel is disabled or the process runs in Home mode. API authentication and filtering remain enforced by `/v0/management/billing/usage`.

## Operational notes

- The ledger is loaded on startup so management queries can aggregate historical events. Use an external usage service for very large or multi-node deployments.
- `sync-on-write: true` fsyncs every event for stronger power-loss durability. The default `false` writes each complete JSONL record immediately and fsyncs during reconfiguration or graceful shutdown.
- Use one CLIProxyAPI process per ledger file. The JSONL store does not coordinate concurrent writers across processes.
- Retries and additional-model calls are separate upstream usage events. This reflects actual provider-side work and avoids undercounting failed-over requests.
- Failed requests with reported token usage are priced normally. Failed requests without usage metadata remain visible with zero tokens and zero cost.
- Unit prices are frozen into each event, so changing configuration affects new requests without rewriting historical charges.
- A ledger cannot mix currencies. To change `currency`, select a new `store-path`; startup rejects a ledger whose recorded currency differs from the configured currency.
- The built-in API intentionally has no destructive ledger endpoint. Archive or rotate the file with your normal retention process.
