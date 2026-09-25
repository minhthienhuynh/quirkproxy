# POST /v1/chat/completions and POST /:provider/v1/chat/completions

Chat over Zen free models, OpenAI-compatible in both directions. The scoped
endpoint (e.g. `/opencode/v1/chat/completions`) restricts to that provider:
unknown/disabled provider or model outside its allowlist → 404 without
touching upstream. The proxy
always calls upstream with `stream:true` (gate requirement): non-stream
requests get the buffered full response; `stream:true` requests get the SSE
stream relayed with the envelope (`id`/`model`/`created`) rewritten, deltas
passed through byte-identical, terminated by exactly one `data: [DONE]`
(emitted by the proxy; an upstream `[DONE]` is not forwarded).

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
| `messages[]` | `role`/`content` verbatim |
| `tools[]` | OpenAI function shape verbatim + gate entries ensured |
| `tool_choice`, `temperature`, `max_tokens` | verbatim when present |
| `stream:true` | SSE relay: envelope rewritten, deltas verbatim, single proxy-emitted `[DONE]` |
| response | `{object:chat.completion, choices:[{message:{role,content,tool_calls?}, finish_reason: stop\|tool_calls}]}` |
| `tool_calls` | aggregated from SSE deltas by index; arguments concatenated in order |

## Errors

Upstream status and message pass through verbatim in
`{error:{message,type,code,hint?}}`; 403 carries `hint: check pinned
User-Agent recipe`. No retry on 4xx. No cache + dead upstream on the
allowlist lookup behaves like `GET /v1/models` (503).

## Notes

- Array message content (`[{type:text,...}]`) keeps text parts joined by
  newline; non-text parts are dropped.
- UA pin rots over time: when opencode CLI releases a new version, verify
  the triple still passes and update `user_agent`.

## Recipe watchdog (optional)

Per-provider `options` (absent = off):

| Option | Meaning |
|---|---|
| `recipe_check_interval` | e.g. `"1h"` — enables a periodic cheapest-probe chat; any failure logs ERROR |
| `recipe_check_model` | model for the probe (default `mimo-v2.6-flash-free`) |
| `zen_api_key` | key sent as Bearer (default `public`); lives only in your private config copy, never committed, never logged |

## Test cross-reference

- Recipe headers/body/IDs: `provider/opencode/chat_test.go` → `TestChatRecipeHeaders`
- SSE aggregation + tool-call merge: `TestChatSSEAggregation`
- 403+hint passthrough, single upstream hit: `TestChatUpstreamErrors`
- Key redaction: `TestRecipeDefaultsAndNoKeyLeak`
- Handler (mapping, 400/404, tool_calls): `server/chat_test.go`
