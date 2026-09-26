# POST /v1/chat/completions and POST /:provider/v1/chat/completions

Chat over free models, OpenAI-compatible in both directions. The scoped
endpoint (e.g. `/opencode/v1/chat/completions`) restricts to that provider:
unknown/disabled provider or model outside its allowlist → 404 without
touching upstream. Models whose `endpoint` is `responses` (e.g. muse-spark)
are served here too via protocol fallback: the proxy calls the responses
backend and translates the result back to the chat shape. The proxy
always calls upstream with `stream:true` (gate requirement): non-stream
requests get the buffered full response; `stream:true` requests get the SSE
stream relayed with the envelope (`id`/`model`/`created`) rewritten, deltas
passed through byte-identical, terminated by exactly one `data: [DONE]`
(emitted by the proxy; an upstream `[DONE]` is not forwarded).
Fallback streams (`stream:true` with a responses-endpoint model) are
buffered instead: one full chunk plus a single proxy-emitted `[DONE]`.

## Recipe (pinned genuine markers, verified live)

- `User-Agent`: `opencode/1.18.30 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.2`
  (override via options `user_agent`; a stale pin fails closed with 403)
- Headers: `Authorization: Bearer <key>` (`zen_api_key` option, default
  `public`), `x-opencode-client: cli`, `x-opencode-project: global`,
  fresh `x-opencode-request` (`msg_` + 12hex + 14base62) and
  `x-opencode-session` (`ses_` + same format) per call
- Body: `stream:true` forced; `tools` must contain `bash` and `read`
  (client tools pass through; skeletal entries appended when absent;
  schemas beyond names are irrelevant to the gate)

## Request/response map

| Direction | Map |
|---|---|
| `model` | must be in the free allowlist, else 404 before upstream |
| `messages[]` | `role`/`content` verbatim on the direct path; via fallback a single message keeps plain text, multi-turn joins `role: content` lines |
| `tools[]` | OpenAI function shape verbatim + gate entries ensured (mapped 1-1 through fallback) |
| `tool_choice`, `temperature`, `max_tokens` | verbatim when present on the direct path; `tool_choice`/`temperature` are dropped on the fallback path (responses wire has no equivalent), `max_tokens` maps to `max_output_tokens`; a truncated responses payload (`status:"incomplete"`) keeps its partial output — empty content when the cut landed before any usable output — and reports `finish_reason: length`, or `content_filter` when its `incomplete_details.reason` is `content_filter` |
| `stream:true` | SSE relay: envelope rewritten, deltas verbatim, single proxy-emitted `[DONE]` |
| response | `{object:chat.completion, choices:[{message:{role,content,tool_calls?}, finish_reason: stop\|tool_calls\|length\|content_filter}]}` |
| `tool_calls` | aggregated from SSE deltas by index; arguments concatenated in order |

## Errors

Upstream status and message pass through verbatim in
`{error:{message,type,code,hint?}}`; 403 carries `hint: check pinned
User-Agent recipe`. No retry on 4xx. No cache + dead upstream on the
allowlist lookup behaves like `GET /v1/models` (503).
Fallback errors pass through the same way. There is no `WrongEndpoint`
rejection: a model served by the other protocol is translated, not refused.
A `status:"failed"` responses payload on the fallback path returns
`502 UpstreamResponseFailed` carrying the upstream message.
A model owned by a provider that cannot serve its endpoint (missing
completer) returns `502 MisconfiguredProvider`. When upstream 429/503
carries `retry-after` (or `retry-after-ms`), the response forwards it as a
`Retry-After` header.

## Notes

- Array message content (`[{type:text,...}]`) keeps text parts joined by
  newline; non-text parts are dropped.
- Fallback drops `reasoning` items when translating responses output back
  to chat (chat shape has no slot for them); every fallback logs
  `model/from_endpoint/to_endpoint` without keys or bodies.
- UA pin rots over time: when opencode CLI releases a new version, verify
  the triple still passes and update `user_agent`.

## Recipe watchdog (optional)

Per-provider `options` (absent = off):

| Option | Meaning |
|---|---|
| `recipe_check_interval` | e.g. `"1h"` — enables a periodic cheapest-probe chat; any failure logs ERROR |
| `recipe_check_model` | model for the probe (default: first model in allowlist) |
| `zen_api_key` | key sent as Bearer (default `public`); lives only in your private config copy, never committed, never logged |

## Test cross-reference

- Recipe headers/body/IDs: `provider/opencode/chat_test.go` → `TestChatRecipeHeaders`
- SSE aggregation + tool-call merge: `TestChatSSEAggregation`
- 403+hint passthrough, single upstream hit: `TestChatUpstreamErrors`
- Key redaction: `TestRecipeDefaultsAndNoKeyLeak`
- Handler (mapping, 400/404, tool_calls): `server/chat_test.go`
- Protocol fallback (chat calling responses models, buffered stream): `server/fallback_test.go` → `TestChatFallbackToResponses`, `server/translate_test.go`
- Truncated fallback output keeps content and reports `finish_reason:"length"` (JSON + stream): `server/fallback_test.go` → `TestChatFallbackIncompleteKeepsTruncatedOutput`, `TestChatFallbackIncompleteStreamFinishReason`
- Truncation before any usable output (empty/reasoning-only) returns empty content + `finish_reason` instead of 502: `server/translate_test.go` → `TestTranslateResponsesPayloadIncompleteEmptyOutput`
- Payload `status` → chat `finish_reason` translation: `server/translate_test.go` → `TestTranslateResponsesPayloadIncompleteFinishReason` (incl. `content_filter`), `TestTranslateResponsesPayloadFailedErrors`
- Failed payload surfaces the upstream error unwrapped (502 `UpstreamResponseFailed`): `server/fallback_test.go` → `TestChatFallbackFailedPayloadSurfacesUpstreamError`
