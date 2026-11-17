# Plans

Active documents:

- [`plugin-architecture-review.md`](plugin-architecture-review.md) — ABI v4
  architecture; M0 core is done and the `headers` path is now strict ABI v4.
- [`m2-v4-only-runtime.md`](m2-v4-only-runtime.md) — completed M2 plan for
  removing the remaining v3 L7 path; `once`/`stream`/`headers` are ABI v4.
- [`m3-v4-lifecycle.md`](m3-v4-lifecycle.md) — completed M3 plan for v4 L4
  lifecycle and removal of v3 from the production import graph.
- [`m4-v4-protocol.md`](m4-v4-protocol.md) — M4 protocol primitives: OUT
  envelope, bounded mutations, state slots and body/response event APIs.
- [`m5-http-integration.md`](m5-http-integration.md) — M5 HTTP/1 framing,
  proxy integration and v4 example/SDK ports.
- [`m6-cleanup.md`](m6-cleanup.md) — final removal of legacy v3 code and
  artifacts after M4/M5 migration.
- [`security-fixes-round2.md`](security-fixes-round2.md) — security fixes from
  round 2 and the remaining DPCP-1 design item.

Archive (implemented, kept for design history):

- [`archive/plugin-http-headers.md`](archive/plugin-http-headers.md) — ABI v3
  `data_mode: headers` design.
- [`archive/plugin-http-headers-implementation.md`](archive/plugin-http-headers-implementation.md)
  — execution plan for the removed ABI v3 headers mode.

All production proxy phases now use ABI v4. Legacy v3 code and examples remain
outside the production import graph until the M6 cleanup.
