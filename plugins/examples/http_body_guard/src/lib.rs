//! `http_body_guard` — stateful HTTP/1.x header & body inspector using ABI v3
//! (`on_data_state` + `on_state_ptr`).
//!
//! It solves the exact problems that stateless `http_guard` and `stream_guard`
//! cannot:
//!   1. Reassembles HTTP request headers split across arbitrary TCP chunks (no
//!      fail-open when a client trickles headers 1 byte at a time);
//!   2. Tracks `Content-Length` across chunks so request bodies are framed
//!      accurately and oversized bodies (> 64 KiB per request or > 128 KiB
//!      cumulative across a connection) are rejected;
//!   3. Keeps a 64-byte rolling tail window across body chunks so an attack
//!      signature (`union select`, `<script`, `${jndi:`, `/etc/passwd`, ...)
//!      split across two `Read()` calls is still caught;
//!   4. Preserves per-connection state *between* HTTP keep-alive requests
//!      (enforcing `MAX_REQUESTS_PER_CONN` and `MAX_CONN_BODY_BYTES`) even
//!      though every chunk may run on a different pooled WASM instance;
//!   5. Scrubs JSON secrets and banner headers in server responses (`dir == 1`).

#![no_std]

use core::panic::PanicInfo;
use wg_common as wg;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

const STATE_MAGIC: u32 = 0x5747_5333; // "WGS3"
const PHASE_HEADERS: u8 = 0;
const PHASE_BODY: u8 = 1;

const HEADER_FIXED: usize = 36;
const TAIL_CAP: usize = 64;
const HDR_ACCUM_OFF: usize = HEADER_FIXED + TAIL_CAP; // 100
const MAX_HDR_ACCUM: usize = 2048;

const MAX_SINGLE_BODY: u64 = 64 * 1024;
const MAX_CONN_BODY_BYTES: u64 = 128 * 1024;
const MAX_REQUESTS_PER_CONN: u64 = 5;

const ALLOWED_METHODS: &[&[u8]] = &[
    b"GET",
    b"HEAD",
    b"POST",
    b"PUT",
    b"PATCH",
    b"DELETE",
    b"OPTIONS",
];

const BLOCKED_PREFIXES: &[&[u8]] = &[
    b"/admin",
    b"/.git",
    b"/.env",
    b"/wp-admin",
    b"/actuator",
];

const SECRET_KEYS: &[&[u8]] = &[
    b"password",
    b"secret",
    b"api_key",
    b"access_token",
    b"refresh_token",
];

struct ReqState {
    phase: u8,
    body_remaining: u64,
    requests_seen: u64,
    total_body_bytes: u64,
    tail_len: usize,
    hdr_len: usize,
}

impl ReqState {
    fn load(raw: &[u8], len: usize) -> Self {
        if len >= HEADER_FIXED && read_u32_le(&raw[0..4]) == STATE_MAGIC {
            let phase = raw[4];
            let body_remaining = read_u64_le(&raw[8..16]);
            let requests_seen = read_u64_le(&raw[16..24]);
            let total_body_bytes = read_u64_le(&raw[24..32]);
            let tail_len = (read_u16_le(&raw[32..34]) as usize).min(TAIL_CAP);
            let hdr_len = (read_u16_le(&raw[34..36]) as usize).min(MAX_HDR_ACCUM);
            Self {
                phase,
                body_remaining,
                requests_seen,
                total_body_bytes,
                tail_len,
                hdr_len,
            }
        } else {
            Self {
                phase: PHASE_HEADERS,
                body_remaining: 0,
                requests_seen: 0,
                total_body_bytes: 0,
                tail_len: 0,
                hdr_len: 0,
            }
        }
    }

    fn save(&self, raw: &mut [u8]) -> usize {
        write_u32_le(&mut raw[0..4], STATE_MAGIC);
        raw[4] = self.phase;
        raw[5] = 0;
        raw[6] = 0;
        raw[7] = 0;
        write_u64_le(&mut raw[8..16], self.body_remaining);
        write_u64_le(&mut raw[16..24], self.requests_seen);
        write_u64_le(&mut raw[24..32], self.total_body_bytes);
        write_u16_le(&mut raw[32..34], self.tail_len as u16);
        write_u16_le(&mut raw[34..36], self.hdr_len as u16);
        if self.hdr_len > 0 {
            HDR_ACCUM_OFF + self.hdr_len
        } else if self.tail_len > 0 {
            HEADER_FIXED + self.tail_len
        } else {
            HEADER_FIXED
        }
    }
}

#[no_mangle]
pub extern "C" fn on_data_ptr() -> i32 {
    wg::scratch_ptr()
}

#[no_mangle]
pub extern "C" fn on_state_ptr() -> i32 {
    wg::state_ptr()
}

#[no_mangle]
pub extern "C" fn on_data_state(
    _conn_id: i64,
    dir: i32,
    state_ptr: i32,
    state_len: i32,
    state_cap: i32,
    data_ptr: i32,
    data_len: i32,
) -> i64 {
    let Some(state_buf) = (unsafe { wg::state_mut(state_ptr, state_cap) }) else {
        return wg::pack_state_verdict(1, 0);
    };
    if state_len < 0 || (state_len as usize) > state_buf.len() {
        return wg::pack_state_verdict(1, 0);
    }

    match dir {
        0 => {
            let Some(payload) = (unsafe { wg::payload(data_ptr, data_len) }) else {
                return wg::pack_state_verdict(1, 0);
            };
            let mut st = ReqState::load(state_buf, state_len as usize);
            if !process_client_chunk(payload, &mut st, state_buf) {
                return wg::pack_state_verdict(1, 0);
            }
            let new_len = st.save(state_buf);
            wg::pack_state_verdict(0, new_len)
        }
        1 => {
            let Some(payload) = (unsafe { wg::payload_mut(data_ptr, data_len) }) else {
                return wg::pack_state_verdict(1, 0);
            };
            let new_len = scrub_response(payload, state_buf, state_len as usize);
            wg::pack_state_verdict(0, new_len)
        }
        _ => wg::pack_state_verdict(1, 0),
    }
}

fn process_client_chunk(payload: &[u8], st: &mut ReqState, state_buf: &mut [u8]) -> bool {
    let mut cursor = 0;
    while cursor < payload.len() {
        if st.phase == PHASE_HEADERS {
            if st.hdr_len == 0 {
                if let Some(rel_end) = wg::find(&payload[cursor..], b"\r\n\r\n") {
                    let hdr_end = cursor + rel_end + 4;
                    let hdr_block = &payload[cursor..hdr_end];
                    if !validate_headers(hdr_block, st) {
                        return false;
                    }
                    cursor = hdr_end;
                    continue;
                }
                let rem = payload.len() - cursor;
                if rem > MAX_HDR_ACCUM {
                    return false;
                }
                state_buf[HDR_ACCUM_OFF..HDR_ACCUM_OFF + rem]
                    .copy_from_slice(&payload[cursor..]);
                st.hdr_len = rem;
                return true;
            }

            // Reassemble fragmented headers byte-by-byte until \r\n\r\n.
            while cursor < payload.len() {
                if st.hdr_len >= MAX_HDR_ACCUM {
                    return false;
                }
                state_buf[HDR_ACCUM_OFF + st.hdr_len] = payload[cursor];
                st.hdr_len += 1;
                cursor += 1;
                if st.hdr_len >= 4
                    && &state_buf[HDR_ACCUM_OFF + st.hdr_len - 4..HDR_ACCUM_OFF + st.hdr_len]
                        == b"\r\n\r\n"
                {
                    let n = st.hdr_len;
                    st.hdr_len = 0;
                    if !validate_headers(&state_buf[HDR_ACCUM_OFF..HDR_ACCUM_OFF + n], st) {
                        return false;
                    }
                    break;
                }
            }
        } else {
            let avail = payload.len() - cursor;
            let take = (avail as u64).min(st.body_remaining) as usize;
            let body_slice = &payload[cursor..cursor + take];

            if !inspect_body_slice(body_slice, st, &mut state_buf[HEADER_FIXED..HEADER_FIXED + TAIL_CAP]) {
                return false;
            }
            cursor += take;
            st.body_remaining -= take as u64;
            if st.body_remaining == 0 {
                st.phase = PHASE_HEADERS;
                st.tail_len = 0;
            }
        }
    }
    true
}

fn validate_headers(hdr_block: &[u8], st: &mut ReqState) -> bool {
    let Some(req) = wg::parse_request(hdr_block) else {
        return false;
    };
    if !ALLOWED_METHODS.iter().any(|m| *m == req.method) {
        return false;
    }
    if wg::contains(req.target, b"../")
        || wg::contains_ci(req.target, b"%2e%2e")
        || BLOCKED_PREFIXES.iter().any(|p| wg::starts_with_ci(req.target, p))
    {
        return false;
    }
    let mut has_host = false;
    let mut cl_count = 0usize;
    let mut content_len: u64 = 0;
    for line in req.headers.split(|&b| b == b'\n') {
        let line = wg::trim_ascii(line);
        if line.is_empty() {
            break;
        }
        let Some(colon) = line.iter().position(|&b| b == b':') else {
            continue;
        };
        let name = wg::trim_ascii(&line[..colon]);
        let val = wg::trim_ascii(&line[colon + 1..]);
        if wg::eq_ci(name, b"Host") {
            has_host = !val.is_empty();
        } else if wg::eq_ci(name, b"Content-Length") {
            cl_count += 1;
            if cl_count > 1 {
                return false;
            }
            let Some(parsed) = wg::parse_u64(val) else {
                return false;
            };
            if parsed > MAX_SINGLE_BODY {
                return false;
            }
            content_len = parsed;
        } else if wg::eq_ci(name, b"Transfer-Encoding") {
            return false;
        }
    }
    if req.version == b"HTTP/1.1" && !has_host {
        return false;
    }

    st.requests_seen = st.requests_seen.saturating_add(1);
    if st.requests_seen > MAX_REQUESTS_PER_CONN {
        return false;
    }
    st.total_body_bytes = st.total_body_bytes.saturating_add(content_len);
    if st.total_body_bytes > MAX_CONN_BODY_BYTES {
        return false;
    }

    st.tail_len = 0;
    if content_len > 0 {
        st.phase = PHASE_BODY;
        st.body_remaining = content_len;
    } else {
        st.phase = PHASE_HEADERS;
        st.body_remaining = 0;
    }
    true
}

fn inspect_body_slice(body: &[u8], st: &mut ReqState, tail: &mut [u8]) -> bool {
    if st.tail_len > 0 && !body.is_empty() {
        let mut stitch = [0u8; TAIL_CAP * 2];
        let tlen = st.tail_len.min(TAIL_CAP);
        let plen = body.len().min(TAIL_CAP);
        stitch[..tlen].copy_from_slice(&tail[..tlen]);
        stitch[tlen..tlen + plen].copy_from_slice(&body[..plen]);
        if has_signature(&stitch[..tlen + plen]) {
            return false;
        }
    }
    if has_signature(body) {
        return false;
    }

    // Update the 64-byte rolling tail window for the next chunk.
    if body.len() >= TAIL_CAP {
        tail.copy_from_slice(&body[body.len() - TAIL_CAP..]);
        st.tail_len = TAIL_CAP;
    } else {
        let keep = (st.tail_len + body.len()).min(TAIL_CAP);
        let old_keep = keep - body.len();
        if old_keep > 0 && old_keep < st.tail_len {
            let shift = st.tail_len - old_keep;
            let mut i = 0;
            while i < old_keep {
                tail[i] = tail[shift + i];
                i += 1;
            }
        }
        tail[old_keep..keep].copy_from_slice(body);
        st.tail_len = keep;
    }
    true
}

fn has_signature(b: &[u8]) -> bool {
    let mut i = 0;
    while i < b.len() {
        let rem = &b[i..];
        let hit = match wg::lower(rem[0]) {
            b'u' => {
                wg::starts_with_ci(rem, b"union select")
                    || wg::starts_with_ci(rem, b"union all select")
            }
            b'o' => wg::starts_with_ci(rem, b"or 1=1") || wg::starts_with_ci(rem, b"onerror="),
            b'<' => wg::starts_with_ci(rem, b"<script"),
            b'j' => wg::starts_with_ci(rem, b"javascript:"),
            b'.' => rem.starts_with(b"../../"),
            b'%' => wg::starts_with_ci(rem, b"%2e%2e%2f"),
            b'$' => wg::starts_with_ci(rem, b"${jndi:"),
            b'/' => rem.starts_with(b"/etc/passwd"),
            b';' => rem.starts_with(b";cat "),
            _ => false,
        };
        if hit {
            return true;
        }
        i += 1;
    }
    false
}

fn scrub_response(payload: &mut [u8], state_buf: &mut [u8], state_len: usize) -> usize {
    let mut responses: u64 = if state_len >= 8 {
        read_u64_le(&state_buf[0..8])
    } else {
        0
    };
    if wg::parse_status(payload).is_some() {
        responses = responses.saturating_add(1);
    }
    for key in SECRET_KEYS {
        mask_json_value(payload, key);
    }
    let _ = wg::mask_header(payload, b"Server", b"wasmgate");
    let _ = wg::mask_header(payload, b"X-Powered-By", b"hidden");
    write_u64_le(&mut state_buf[0..8], responses);
    8
}

fn mask_json_value(buf: &mut [u8], key: &[u8]) -> bool {
    let mut from = 0;
    while from < buf.len() {
        let Some(q1) = wg::find(&buf[from..], b"\"") else {
            return false;
        };
        let kstart = from + q1 + 1;
        let Some(q2) = wg::find(&buf[kstart..], b"\"") else {
            return false;
        };
        let kend = kstart + q2;
        if wg::eq_ci(&buf[kstart..kend], key) {
            mask_after_colon(buf, kend + 1);
        }
        from = kend + 1;
    }
    false
}

fn mask_after_colon(buf: &mut [u8], mut i: usize) {
    while let Some(&c) = buf.get(i) {
        if c == b' ' || c == b'\t' || c == b'\r' || c == b'\n' {
            i += 1;
        } else {
            break;
        }
    }
    if buf.get(i) != Some(&b':') {
        return;
    }
    i += 1;
    while let Some(&c) = buf.get(i) {
        if c == b' ' || c == b'\t' || c == b'\r' || c == b'\n' {
            i += 1;
        } else {
            break;
        }
    }
    if buf.get(i) != Some(&b'"') {
        return;
    }
    i += 1;
    while let Some(&c) = buf.get(i) {
        if c == b'"' || c == b'\n' {
            break;
        }
        buf[i] = b'*';
        i += 1;
    }
}

fn read_u16_le(b: &[u8]) -> u16 {
    (b[0] as u16) | ((b[1] as u16) << 8)
}

fn write_u16_le(b: &mut [u8], v: u16) {
    b[0] = v as u8;
    b[1] = (v >> 8) as u8;
}

fn read_u32_le(b: &[u8]) -> u32 {
    (b[0] as u32) | ((b[1] as u32) << 8) | ((b[2] as u32) << 16) | ((b[3] as u32) << 24)
}

fn write_u32_le(b: &mut [u8], v: u32) {
    b[0] = v as u8;
    b[1] = (v >> 8) as u8;
    b[2] = (v >> 16) as u8;
    b[3] = (v >> 24) as u8;
}

fn read_u64_le(b: &[u8]) -> u64 {
    (b[0] as u64)
        | ((b[1] as u64) << 8)
        | ((b[2] as u64) << 16)
        | ((b[3] as u64) << 24)
        | ((b[4] as u64) << 32)
        | ((b[5] as u64) << 40)
        | ((b[6] as u64) << 48)
        | ((b[7] as u64) << 56)
}

fn write_u64_le(b: &mut [u8], v: u64) {
    b[0] = v as u8;
    b[1] = (v >> 8) as u8;
    b[2] = (v >> 16) as u8;
    b[3] = (v >> 24) as u8;
    b[4] = (v >> 32) as u8;
    b[5] = (v >> 40) as u8;
    b[6] = (v >> 48) as u8;
    b[7] = (v >> 56) as u8;
}
