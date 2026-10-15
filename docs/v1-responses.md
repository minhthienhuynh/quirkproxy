# POST /v1/responses and POST /:provider/v1/responses

Responses API over free models (currently `muse-spark-1.3-contributor-free`).
The scoped endpoint (e.g. `/opencode/v1/responses`) restricts to that provider:
unknown/disabled provider or model outside its allowlist → 404 without
touching upstream. Models whose `endpoint` is `chat/completions`
are served here too via protocol fallback: the proxy calls the chat
backend and builds a Responses payload back. The proxy always calls upstream with `stream:true` (gate
requirement): non-stream requests get the buffered full response (waits for
a terminal event — `response.completed`, or `response.incomplete` when the
model hits `max_output_tokens`; returns the response object with envelope
rewritten; `response.failed` surfaces as 502 with the upstream message);
`stream:true` requests get the SSE stream relayed with `event:` lines preserved,
envelope (`id`/`model`/`created_at`) rewritten inside `response` objects,
output items passed through byte-identical, terminated by exactly one
`event: response.completed` (emitted by the proxy; an upstream
`response.completed` is swallowed) — unless the stream ends on a relayed
`response.incomplete`/`response.failed`, which terminates it directly.
Fallback streams (`stream:true` with a chat-endpoint model) are
buffered instead: `response.created` plus a single proxy-emitted
`response.completed`.

## Recipe (shared with chat, pinned genuine markers, verified live)

Same recipe as `POST /v1/chat/completions` — see `docs/v1-chat.md` §Recipe.
One recipe covers both endpoints; a change to the recipe applies to both.

## Request/response map

| Direction | Map |
|---|---|
| `model` | must be in the free allowlist, else 404 before upstream |
| `input` | string or items array, passed through verbatim on the direct path; via fallback a string becomes one user message, an array maps message/input_text items keeping role |
| `tools[]` | Responses function shape (`{type:"function",name,description,parameters}`) verbatim + gate entries (`shell`, `read`, `glob`, `grep`) ensured (mapped 1-1 through fallback) |
| `reasoning` | verbatim when present on the direct path; dropped on the fallback path (chat wire has no equivalent) |
| `max_output_tokens` | verbatim when present on the direct path; maps to `max_tokens` on the fallback path |
| `stream:true` | SSE relay: `event:` lines preserved, envelope rewritten in `response` objects, output items byte-identical, single proxy-emitted `response.completed` |
| `stream:false` | proxy calls upstream `stream:true`, waits for a terminal event (`response.completed` or `response.incomplete`), returns the response object with envelope rewritten; `response.failed` → 502 with the upstream message |
| response envelope | `id` (proxy-generated `resp_` + 26 base62), `model`, `created_at` rewritten; `object`, `status`, `output[]` and all other fields pass through |

## SSE event types

| Event | Relay behavior |
|---|---|
| `response.created` | relayed; `response.id`/`response.model`/`response.created_at` rewritten |
| `response.in_progress` | relayed; `response` envelope rewritten |
| `response.output_item.added` | relayed as-is (item passed through byte-identical) |
| `response.output_item.done` | relayed as-is (item passed through byte-identical) |
| `response.content_part.added` | relayed as-is |
| `response.output_text.delta` | relayed as-is |
| `response.content_part.done` | relayed as-is |
| `response.completed` | swallowed; exactly one proxy-emitted `response.completed` ends the stream |
| `response.incomplete` | relayed (envelope rewritten); terminal — ends the stream (truncation: `incomplete_details.reason` is `max_output_tokens` or `content_filter`) |
| `response.failed` | relayed (envelope rewritten); terminal — ends the stream |
| `ping` | relayed as-is |

Output items (`reasoning`, `function_call`, `message`) pass through
byte-identical. `reasoning.encrypted_content` is opaque — the proxy does not
decode or normalize it.

## Errors

Upstream status and message pass through verbatim in
`{error:{message,type,code,hint?}}`; 403 carries `hint: check pinned
User-Agent recipe`. No retry on 4xx. No cache + dead upstream on the
allowlist lookup behaves like `GET /v1/models` (503).
Fallback errors pass through the same way. There is no `WrongEndpoint`
rejection: a model served by the other protocol is translated, not refused.

| Situation | HTTP |
|---|---|
| Model not in free allowlist | 404 before upstream |
| Unknown/disabled provider (scoped route) | 404 |
| Model owned by a provider that cannot serve its endpoint | 502 MisconfiguredProvider |
| Upstream 4xx | verbatim status + message (403 + hint) |
| Upstream `response.failed` (buffered) | 502 UpstreamResponseFailed + upstream message |
| Inference timeout (generation bound exceeded) | 502 UpstreamTimeout + cause |
| Transport failure (dial/reset/connection) | 502 UpstreamTransport + cause |
| No cache + dead upstream (allowlist) | 503 |
| Usable cache + dead upstream (allowlist) | 502 |

Inference calls are bounded by the request context plus a configurable
generation timeout (see `docs/v1-chat.md` options), never by a fixed total
request timeout — long generations are not cut off mid-stream. Timeout and
transport failures carry the original cause in `message`; only unclassifiable
internal errors fall back to the generic `"chat upstream unavailable"` string.

Upstream `retry-after` (or `retry-after-ms`) on 429/503 is forwarded as a
`Retry-After` response header.

## Notes

- The proxy never runs a function_call loop; the client loops over
  `function_call` output items itself.
- `reasoning.encrypted_content` is opaque and only meaningful when passed
  back to the same upstream in a follow-up request.
- The watchdog (`recipe_check_interval`) probes both chat and responses
  endpoints in the same loop; a gate change on either logs ERROR with
  `kind=chat` or `kind=responses`.

## Test cross-reference

- Recipe headers/body/IDs: `provider/opencode/responses_test.go` → `TestResponsesRecipeHeaders`
- Field passthrough (input/reasoning/max_output_tokens): `TestResponsesFieldPassthrough`
- Non-stream aggregation + reasoning item pass-through: `TestResponsesAggregation`
- 403+hint / 401 passthrough, no 4xx retry: `TestResponsesUpstreamErrors`
- No terminal event → error: `TestResponsesNoCompletedEvent`
- Terminal `response.incomplete` folds (truncation kept, envelope rewritten, output + `incomplete_details` pass-through): `TestResponsesIncompleteEventFolds`
- `response.failed` → 502 with the upstream message: `TestResponsesFailedEventSurfacesError`
- Handler (non-stream, stream relay, 404, 403, 503, scoped): `server/responses_test.go`
- Protocol fallback (responses calling chat models, buffered stream): `server/fallback_test.go` → `TestResponsesFallbackToChat`
- Watchdog responses probe + gate-change detection: `provider/watch_test.go` → `TestWatchRecipeResponsesGateChange`
