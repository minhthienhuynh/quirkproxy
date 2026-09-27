# quirkproxy

OpenAI-compatible proxy that exposes **free** AI models with full
configuration metadata. First provider: OpenCode Zen.

## Quickstart

```bash
make build
./quirkproxy -config config.sample.json
```

Or `make run` (same thing). Verify with `make smoke`; full checks with
`make vet`, `make fmt`, `make test` (`make race` for concurrent code).

- `GET http://127.0.0.1:8080/healthz` → `{"status":"ok"}`
- `GET http://127.0.0.1:8080/v1/models` → free models from all enabled providers
- `GET http://127.0.0.1:8080/:provider/v1/models` → free models of one
  provider only (e.g. `/opencode/v1/models`); unknown or disabled
  provider → `404`
- `POST http://127.0.0.1:8080/v1/chat/completions` → chat over free
  models (OpenAI shape in and out; `stream:true` relayed as SSE).
  Scoped variant: `POST /:provider/v1/chat/completions`.
  Details: `docs/v1-chat.md`.

Point any OpenAI-compatible client at `http://127.0.0.1:8080/v1`
as its base URL. Flags: `-config <path>` (default `config.sample.json`).

## Configuration

See `config.sample.json`. Each provider shares one envelope:

- `id`, `type`, `enabled`
- `blacklist` / `whitelist` (model ids)
- `refresh_ttl` + per-source `refresh` overrides, `max_cache_age`
- `experimental` (unused — `alpha` models are always included like CLI)
- `options` (provider-specific, e.g. upstream URLs)
- Chat recipe options: `zen_api_url`, `user_agent` (pinned genuine triple),
  `client`, `project`, `zen_api_key` (default `public`; private copies only),
  `recipe_check_interval` / `recipe_check_model` (optional watchdog)
- Inference timeout options (duration strings): `inference_generation_timeout`
  (default `10m`, safety-net bound per call) and `inference_header_timeout`
  (default `30s`, wait for upstream headers). Inference is never cut off by a
  fixed total request timeout.

Bind address can be overridden at runtime without editing the file (env wins
over `listen`): `QUIRKPROXY_HOST` and `QUIRKPROXY_PORT`. Set only one to keep the
other from `listen`.

## API

`GET /v1/models` returns the OpenAI core shape (`id`, `object`,
`created`, `owned_by`) plus extensions: `name`, `context_length`,
`max_completion_tokens`, `pricing`, `capabilities` (tool_call,
reasoning, attachment, temperature, modalities, interleaved),
`variants` (reasoning effort levels), and a `quirk` debug namespace
(`status`, `family`, `transport`, `endpoint`).

Response header `X-Quirk-Cache-Age` reports data age in seconds.
No usable cache + dead upstream → `503` (never an empty list).
Field sources are documented in `docs/v1-models.md`.

## Data sources (opencode provider)

- Zen catalog: `GET https://opencode.ai/zen/v1/models` (public, ~1h TTL)
- Metadata: `GET https://models.dev/api.json`, `opencode` entry
  (public, ~12h TTL)

Filtering mirrors the opencode CLI: keep `cost.input == 0`, drop
`deprecated` only (`alpha` kept like CLI), intersect the Zen
catalog fail-closed, then apply blacklist/whitelist. Variants come
from models.dev `reasoning_options`.

## Repository layout

```
main.go               wiring: config → registry → server
server/               Fiber app, routes, OpenAI mapping
config/               shared config envelope
model/                normalized internal model
provider/             ModelLister interface, registry, shared
                      cache (stale-while-revalidate), retry, diagnostics
provider/opencode/    Zen free-models provider (+ testdata fixtures)
docs/                 endpoint documentation
openspec/             OpenSpec change history (zen-free-models)
```

Adding a provider = new package implementing `ModelLister` +
one registration line. No reflection, no plugins.

## Scope and limits

- Inference covers `/v1/chat/completions` and `/v1/responses`.
- No auth on the proxy itself — bind `127.0.0.1` unless you
  put it behind a reverse proxy with TLS.
- Logs go to stderr (slog text).

## Deployment

Push `main` to trigger CI (gates → GHCR image → Dokploy deploy).
Bind address can be overridden at runtime via `QUIRKPROXY_HOST` /
`QUIRKPROXY_PORT` (see `.env.example`); keep the proxy behind a reverse
proxy with TLS — it has no auth of its own.
