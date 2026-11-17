# M5 — HTTP integration and v4 examples

- **Status:** in progress (HTTP/1 Content-Length slice implemented)
- **Depends on:** M4 protocol primitives

- Add explicit opt-in HTTP/1 request/response framing policy (`body_policy`).
- Feed request body, response headers and response body phases from the proxy.
- Preserve `splice(2)` when no body/response policy is selected.
- Define Content-Length fail-closed rules. M5 intentionally rejects
  Transfer-Encoding/chunked and duplicate/invalid Content-Length; chunked
  framing is a separate follow-up, not a silently partial implementation.
- Add the v4 Rust SDK and port the reference examples (remaining M5 work).
- Add live proxy tests for mutation, state, split framing and response policy.

No v3 fallback is allowed. Unsupported framing or a v3 module is a load-time
error.

## Implemented slice

- `body_policy: request|response|both` requires `data_mode: headers`;
- request body, response headers and response body phases are dispatched by the
  proxy for Content-Length messages;
- malformed/ambiguous framing fails closed before upstream use;
- request/response body chunks may receive same-length OUT mutations; insertion
  and deletion remain rejected.

The remaining M5 work is the v4 Rust SDK and example ports. Until those ports
exist, the committed v3 `.wasm` files remain intentionally rejected by the
loader and are not used by showcase configurations.
