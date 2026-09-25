# AGENTS.md

Instructions for AI agents working in this repo. Not a human tutorial.

## Commands

- Standard entry points (use these): `make build`, `make test`,
  `make race`, `make vet`, `make fmt`, `make run`, `make smoke`, `make clean`.
  Raw `go` commands only when make lacks the flag you need.
- Gate before done: `make vet` clean, `make fmt` empty, `make test` green.
  Touching shared/concurrent code? `make race` instead of `make test`.
- Run: `make run` (`:8080`, config `config.sample.json`).
  Smoke: `make smoke` (healthz + models, pid in `/tmp/qp.pid`).
- Never commit the `quirkproxy` binary to root (gitignored, `make clean` removes it).
- Stack: Go 1.27.1 + Fiber **v3**. v3 API differs from v2 — check `go doc` before assuming Config fields exist.

## Fetching model data from providers

Listing never needs auth or the genuine binary. Two public read-only sources:

- Zen catalog: `GET https://opencode.ai/zen/v1/models` → `{object:"list", data:[{id, created, owned_by}]}`. No key required. Refresh TTL ~1h.
- Metadata: `GET https://models.dev/api.json` → top-level map, read only the `<provider-id>` entry (don't parse the whole 5MB file into domain structs). Refresh TTL ~12h.

Per-model transport rule (mirrors opencode CLI `fromModelsDevModel`):
`npm = model.provider.npm ?? provider.npm ?? "@ai-sdk/openai-compatible"`;
`url = model.provider.api ?? provider.api`. `npm == "@ai-sdk/openai"` means the `responses` endpoint, otherwise `chat/completions`.

Variants rule (mirrors CLI `reasoningVariants`, `reasoning_options` only):
effort → values verbatim; toggle → low/medium/high; budget_tokens → high/max;
`[]`/absent → `{}` (this short-circuits the CLI family matrix — do not port it).

Free rule (mirrors CLI, fail-closed where noted):
`cost.input == 0` → drop `deprecated` (and `alpha` unless experimental) →
must exist in provider catalog (absent → drop + log; this also excludes
models missing from models.dev entirely) → blacklist/whitelist.
Null `cost` in models.dev → drop (CLI defaults it to 0; we fail closed instead).

## When a provider updates models

1. Re-fetch both sources live and diff ids against the current output.
2. Ground truth for opencode: `opencode models opencode --verbose` — ids + variant names must match `curl /v1/models` 100%. Any diff must have a drop-log reason; unexplained diff = bug.
3. Refresh fixtures: overwrite `provider/opencode/testdata/zen-models.json` with the
   live catalog; regenerate the trimmed models.dev fixture keeping all free
   models plus at least one paid, one deprecated, and one cost-null example.
   Then update the expected id set in `merge_test.go` — never fake fixture data.
4. Free lists rotate ("limited time"). Never hardcode an allowlist; filtering
   must stay dynamic (cost + status + catalog intersection).

## Adding a provider

- New package `provider/<name>/` implementing `ModelLister` (ID, Capabilities,
  ListModels → ListResult). Reuse `provider.Refresher`, `Retry`, `LogDrops`, `DropReason`.
- Map that provider's own "free" definition onto `model.Model`. Always set
  `Owner` (drives `owned_by`); never hardcode owner or endpoint names in `server/`.
- Wire: one `case` + import in `main.go`, one config block (`type: <name>`).
- Do not touch `server/`, `config/`, or existing providers.
- Duplicate ids across providers resolve first-wins by lowest provider id (sorted) + WARN log.

## Docs rule (agent contract)

- `docs/v1-models.md` is the field-source contract, not a human tutorial.
  When changing any response shape, update its field-source table and the
  test cross-reference in the same commit. A shape change without a docs
  update is an incomplete change.
- `README.md` stays thin and human-facing. Detailed behavior lives in
  `docs/` and OpenSpec specs, not in the README.

## Operational gotchas

- Port 8080 is often held by a stale instance: `bind: address already in use`
  in the log means curl is hitting the old server. Kill all (`kill $(cat /tmp/qp*.pid)`) and restart.
- Fiber matches routes in registration order: `/v1/models` must stay registered
  before `/:provider/v1/models`.
- `server/` must stay provider-agnostic. Provider-specific names in shared
  structs (`model.Model`, response fields) are a review blocker — namespace them
  per provider instead.
