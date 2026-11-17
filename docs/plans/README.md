# Plans

Active documents:

- [`plugin-architecture-review.md`](plugin-architecture-review.md) — ABI v4
  architecture; M0 core is done and the `headers` path is now strict ABI v4.
- [`security-fixes-round2.md`](security-fixes-round2.md) — security fixes from
  round 2 and the remaining DPCP-1 design item.

Archive (implemented, kept for design history):

- [`archive/plugin-http-headers.md`](archive/plugin-http-headers.md) — ABI v3
  `data_mode: headers` design.
- [`archive/plugin-http-headers-implementation.md`](archive/plugin-http-headers-implementation.md)
  — execution plan for the removed ABI v3 headers mode.

`data_mode: headers` now requires ABI v4. `once` and `stream` remain explicit
legacy ABI v3 modes until their v4 phases are implemented.
