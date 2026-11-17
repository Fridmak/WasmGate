# M4 — ABI v4 protocol primitives

- **Status:** complete
- **Depends on:** M3 lifecycle cutover

The previous plan put envelope output, state, HTTP framing and Rust example
ports into one milestone. That is too broad to validate safely. M4 is therefore
limited to the ABI contract and engine primitives; M5 integrates them into the
HTTP proxy.

## M4.1 — OUT envelope (done)

- Add a versioned OUT envelope with bounded mutation and state sections.
- Add `CapMutation` and require explicit output-window exports only when a plan
  can produce output.
- Host never copies output unless the guest returns a valid OUT envelope.
- Malformed output, oversized output and unknown mutation flags fail closed.

## M4.2 — state slots (done)

- `Event.State` is the input state slot.
- OUT state replaces the slot only after a successful verdict.
- State is bounded, connection-owned and never shared by pooled instances.
- No implicit state for opaque M2 paths; M5 chooses the proxy ownership model.

## M4.3 — body/response event API (done)

- Add typed engine entry points for request body, response headers and response
  body phases.
- They use the same envelope/plan/timeout/verdict machinery as headers/opaque.
- No HTTP framing is implemented in M4.

## Acceptance

- round-trip IN/OUT conformance tests;
- mutation/state bounds and corruption tests;
- timeout and failed-output tests;
- body/response phase dispatch tests;
- fuzz OUT parsing;
- full tests, vet, build and race.

M5 owns HTTP/1 framing and proxy integration. M6 owns deletion of the legacy v3
package and artifacts after the v4 examples are ported.
