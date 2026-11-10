//! `protocol_router` — first-packet protocol detection and pool routing.
//!
//! The nginx `stream` + `map $protocol` pattern, implemented as a WASM plugin.
//! It recognises TLS, HTTP/1.x, SSH, PostgreSQL and Redis by their first bytes
//! and returns a route index:
//!
//! ```yaml
//! plugin:
//!   path: "plugins/examples/protocol_router/protocol_router.wasm"
//!   data_mode: "once"
//!   route_pools: ["tls", "http", "ssh", "postgres", "redis"]
//! ```
//!
//! verdict 0 = default pool, 1 = deny, 2..6 = route_pools[0..4]. Unknown
//! protocols are denied by `DENY_UNKNOWN`, which is the useful setting for a
//! gateway that should only expose known services.

#![no_std]

use core::panic::PanicInfo;
use wg_common as wg;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

const VERDICT_DEFAULT: i32 = 0;
const VERDICT_DENY: i32 = 1;
const ROUTE_TLS: i32 = 2;
const ROUTE_HTTP: i32 = 3;
const ROUTE_SSH: i32 = 4;
const ROUTE_POSTGRES: i32 = 5;
const ROUTE_REDIS: i32 = 6;

const DENY_UNKNOWN: bool = true;

const PG_PROTOCOL_V3: u32 = 196_608; // 0x0003_0000
const PG_SSL_REQUEST: u32 = 80_877_103; // 0x04d2_162f
const PG_GSSENC_REQUEST: u32 = 80_877_102; // 0x04d2_162e

#[no_mangle]
pub extern "C" fn on_data_ptr() -> i32 {
    wg::scratch_ptr()
}

#[no_mangle]
pub extern "C" fn on_data(ptr: i32, len: i32) -> i32 {
    let Some(payload) = (unsafe { wg::payload(ptr, len) }) else {
        return VERDICT_DENY;
    };
    if payload.is_empty() {
        return if DENY_UNKNOWN { VERDICT_DENY } else { VERDICT_DEFAULT };
    }

    if is_tls(payload) {
        return ROUTE_TLS;
    }
    if wg::looks_like_http(payload) {
        return ROUTE_HTTP;
    }
    if payload.starts_with(b"SSH-") {
        return ROUTE_SSH;
    }
    if is_postgres(payload) {
        return ROUTE_POSTGRES;
    }
    if is_redis(payload) {
        return ROUTE_REDIS;
    }

    if DENY_UNKNOWN {
        VERDICT_DENY
    } else {
        VERDICT_DEFAULT
    }
}

fn is_tls(b: &[u8]) -> bool {
    b.len() >= 3 && b[0] == 0x16 && b[1] == 0x03
}

fn is_postgres(b: &[u8]) -> bool {
    if b.len() < 8 {
        return false;
    }
    let len = match wg::be32(&b[0..4]) {
        Some(v) => v,
        None => return false,
    };
    let code = match wg::be32(&b[4..8]) {
        Some(v) => v,
        None => return false,
    };
    let sane_len = len >= 8 && len <= 1 << 20;
    sane_len && (code == PG_PROTOCOL_V3 || code == PG_SSL_REQUEST || code == PG_GSSENC_REQUEST)
}

fn is_redis(b: &[u8]) -> bool {
    if b.first() == Some(&b'*') {
        return true; // RESP array (the normal client wire format)
    }
    const INLINE: &[&[u8]] = &[
        b"PING",
        b"AUTH ",
        b"HELLO ",
        b"GET ",
        b"SET ",
        b"MGET ",
        b"INFO",
        b"COMMAND",
        b"SELECT ",
    ];
    INLINE.iter().any(|cmd| wg::starts_with_ci(b, cmd))
}
