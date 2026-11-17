# M5 — HTTP integration and v4 examples

- **Status:** planned
- **Depends on:** M4 protocol primitives

- Add explicit opt-in HTTP/1 request/response framing policy.
- Feed request body, response headers and response body phases from the proxy.
- Preserve `splice(2)` when no body/response policy is selected.
- Define Content-Length/chunked/keep-alive and smuggling fail-closed rules.
- Add the v4 Rust SDK and port the reference examples.
- Add live proxy tests for mutation, state, split framing and response policy.

No v3 fallback is allowed. Unsupported framing or a v3 module is a load-time
error.
