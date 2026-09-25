# POST /v1/responses and POST /:provider/v1/responses

Responses API over Zen free models (currently `muse-spark-1.3-contributor-free`).
The scoped endpoint (e.g. `/opencode/v1/responses`) restricts to that provider:
unknown/disabled provider or model outside its allowlist → 404 without
touching upstream. The proxy always calls upstream with `stream:true` (gate
requirement): non-stream requests get the buffered full response (waits for
`response.completed`, returns the response object with envelope rewritten);
`stream:true` requests get the SSE stream relayed with `event:` lines preserved,
envelope (`id`/`model`/`created_at`) rewritten inside `response` objects,
output items passed through byte-identical, terminated by exactly one
`event: response.completed` (emitted by the proxy; an upstream
`response.completed` is swallowed).

## Recipe (shared with chat, pinned genuine markers, verified live)

Same recipe as `POST /v1/chat/completions` — see `docs/v1-chat.md` §Recipe.
One recipe covers both endpoints; a change to the recipe applies to both.

## Request/response map

| Direction | Map |
|---|---|
| `model` | must be in the free allowlist, else 404 before upstream |
| `input` | string or items array, passed through verbatim |
| `tools[]` | Responses function shape (`{type:"function",name,description,parameters}`) verbatim + gate entries (`bash`, `read`) ensured |
| `reasoning` | verbatim when present |
| `max_output_tokens` | verbatim when present |
| `stream:true` | SSE relay: `event:` lines preserved, envelope rewritten in `response` objects, output items byte-identical, single proxy-emitted `response.completed` |
| `stream:false` | proxy calls upstream `stream:true`, waits for `response.completed`, returns the response object with envelope rewritten |
| response envelope | `id` (proxy-generated `resp-` form), `model`, `created_at` rewritten; `object`, `status`, `output[]` and all other fields pass through |

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
| `ping` | relayed as-is |

Output items (`reasoning`, `function_call`, `message`) pass through
byte-identical. `reasoning.encrypted_content` is opaque — the proxy does not
decode or normalize it.

## Errors

Upstream status and message pass through verbatim in
`{error:{message,type,code,hint?}}`; 403 carries `hint: check pinned
User-Agent recipe`. No retry on 4xx. No cache + dead upstream on the
allowlist lookup behaves like `GET /v1/models` (503).

| Situation | HTTP |
|---|---|
| Model not in free allowlist | 404 before upstream |
| Unknown/disabled provider (scoped route) | 404 |
| Upstream 4xx | verbatim status + message (403 + hint) |
| No cache + dead upstream (allowlist) | 503 |
| Usable cache + dead upstream (allowlist) | 502 |

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
- Missing `response.completed` → error: `TestResponsesNoCompletedEvent`
- Handler (non-stream, stream relay, 404, 403, 503, scoped): `server/responses_test.go`
- Watchdog responses probe + gate-change detection: `provider/watch_test.go` → `TestWatchRecipeResponsesGateChange`
