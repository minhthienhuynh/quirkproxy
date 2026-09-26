# GET /v1/models and GET /:provider/v1/models

List free models in an OpenAI-compatible shape with extension fields.
Every field traces to a source — no invented values.

The aggregate endpoint (`/v1/models`) merges all enabled providers;
duplicate ids keep the lowest provider id (sorted) + WARN log.
The scoped endpoint (`/:provider/v1/models`, e.g. `/opencode/v1/models`)
returns only that provider's models; unknown or disabled provider → 404.

## Response

`{object:"list", data:[item...]}`. The `X-Quirk-Cache-Age` header reports
data age in seconds.

## Fields and sources

| Field | Source |
|---|---|
| `id`, `object`, `created`, `owned_by` | `created` from the Zen catalog (`GET /zen/v1/models`); `owned_by` from the provider (`Owner`, currently opencode); rest fixed |
| `name` | models.dev `name` |
| `context_length`, `max_completion_tokens` | models.dev `limit.context`, `limit.output` |
| `pricing.prompt`, `pricing.completion`, `pricing.cache_read` | models.dev `cost.input`, `cost.output`, `cost.cache_read` (per 1M tokens; free = 0) |
| `capabilities.tool_call` | models.dev `tool_call` (absent → `true`, per CLI) |
| `capabilities.reasoning`, `capabilities.attachment`, `capabilities.temperature` | models.dev same name (absent → `false`, per CLI) |
| `capabilities.modalities` | models.dev `modalities.input/output` |
| `capabilities` interleaved | models.dev `interleaved` (string or `{"field": ...}`; absent → field omitted) |
| `variants` | models.dev `reasoning_options`: effort → values verbatim; toggle → low/medium/high; budget_tokens → high/max; empty/absent → `{}` |
| `quirk.status` | `"active"` after filtering (deprecated already dropped; alpha kept like CLI) |
| `quirk.family` | models.dev `family` |
| `quirk.transport` | `model.provider.npm ?? provider.npm ?? "@ai-sdk/openai-compatible"` (per CLI) |
| `quirk.endpoint` | `"responses"` when transport is `@ai-sdk/openai`, otherwise `"chat/completions"` (Zen-specific; other providers define their own). Both `/v1/chat/completions` and `/v1/responses` serve either endpoint via protocol fallback, so clients may ignore this field for routing and use it only for debugging |

## Filtering (CLI mirror, fail-closed)

`cost.input == 0` (null cost = free) → drop `deprecated` only →
intersect Zen catalog (absent → drop + log) → blacklist/whitelist.
Models missing `cost` in models.dev are treated as free (`cost.input = 0`).
Every dropped model logs exactly one line with its filter step.
Duplicate ids across providers: keep the lowest provider id (sorted), drop the rest + WARN log.

## Errors

| Situation | HTTP |
|---|---|
| No cache + dead upstream | 503 with a clear message |
| Usable cache within max age + dead upstream | 200 with stale cache |
| Other internal error | 502 |

## Test cross-reference

- OpenAI core shape: `server/server_test.go` → `TestModelsOpenAICompatible`
- Enriched fields + cache-age header: same test
- 503/502: `TestModelsUpstreamDownNoCache`, `TestModelsUnexpectedError`
- Known 7-model set + drop reasons + blacklist: `provider/opencode/merge_test.go`
- Variants matching the CLI: `TestMergeVariantsMatchCLI`
- Defensive fetching + real fixtures: `provider/opencode/fetch_test.go`
