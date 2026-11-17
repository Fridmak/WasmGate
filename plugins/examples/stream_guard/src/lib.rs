//! `stream_guard` demonstrates read-only opaque inspection (`data_mode: stream`).
//! Reads are not message boundaries. A signature split across reads is not
//! detectable here; use a framed body policy for the supported HTTP/1 slice.

#![no_std]

use wg_common::{self as wg, v4};

wg::wg_v4_plugin!(v4::CAP_OPAQUE, inspect);

const REQUEST_SIGNATURES: &[&[u8]] = &[
    b"union select",
    b"union all select",
    b"or 1=1",
    b"' or '",
    b"<script",
    b"javascript:",
    b"onerror=",
    b"onload=",
    b"../../",
    b"..%2f",
    b"%2e%2e%2f",
    b"${jndi:",
    b"/etc/passwd",
    b";cat ",
    b"benchmark(",
    b"sleep(",
    b"pg_sleep(",
    b"information_schema",
];

fn inspect(call: v4::Call, event: v4::Event<'_>, _: &mut v4::Output<'_>) -> i32 {
    if call.phase != v4::Phase::Opaque {
        return v4::DENY;
    }
    if call.flags.contains(v4::Flags::CLIENT_TO_SERVER)
        && REQUEST_SIGNATURES
            .iter()
            .any(|sig| wg::contains_ci(event.blob(), sig))
    {
        return v4::DENY;
    }
    v4::ALLOW
}
