//! `http_body_guard` uses the host's first-exchange HTTP/1 framing.
//! It limits request size, scans body chunks with a rolling tail, and
//! demonstrates same-length response replacements for opted-in plain text.

#![no_std]

use wg_common::{self as wg, v4};

wg::wg_v4_plugin!(
    v4::CAP_HTTP_HEADERS | v4::CAP_STATE_IN | v4::CAP_STATE_OUT | v4::CAP_MUTATION,
    inspect
);

const TAIL: usize = 64;
const REPLACE_CHUNK: usize = 2 * 1024;
const MAX_REQUEST: u64 = 64 * 1024;
const MASK_STATE: &[u8] = b"digits";

const ALLOWED_METHODS: &[&[u8]] = &[
    b"GET", b"HEAD", b"POST", b"PUT", b"PATCH", b"DELETE", b"OPTIONS",
];
const BLOCKED_PREFIXES: &[&[u8]] = &[b"/admin", b"/.git", b"/.env", b"/wp-admin", b"/actuator"];
const SIGNATURES: &[&[u8]] = &[
    b"union select",
    b"union all select",
    b"or 1=1",
    b"onerror=",
    b"<script",
    b"javascript:",
    b"../../",
    b"%2e%2e%2f",
    b"${jndi:",
    b"/etc/passwd",
    b";cat ",
];

fn inspect(call: v4::Call, event: v4::Event<'_>, out: &mut v4::Output<'_>) -> i32 {
    if call.flags.contains(v4::Flags::TRUNCATED) {
        return v4::DENY;
    }
    let ok = match call.phase {
        v4::Phase::RequestHeaders => request_head(&event),
        v4::Phase::RequestBody => request_body(&event, call.flags, out),
        v4::Phase::ResponseHeaders => response_head(&event, out),
        v4::Phase::ResponseBody => response_body(&event, out),
        v4::Phase::Opaque => false,
    };
    if ok {
        v4::ALLOW
    } else {
        v4::DENY
    }
}

fn request_head(event: &v4::Event<'_>) -> bool {
    let Some(req) = event.request_line() else {
        return false;
    };
    let headers = event.headers();
    ALLOWED_METHODS.contains(&req.method)
        && !wg::contains(req.target, b"../")
        && !wg::contains_ci(req.target, b"%2e%2e")
        && !BLOCKED_PREFIXES
            .iter()
            .any(|p| wg::starts_with_ci(req.target, p))
        && (req.version != b"HTTP/1.1"
            || headers
                .first(b"Host")
                .is_some_and(|v| !wg::trim_ascii(v).is_empty()))
        && headers.first(b"Transfer-Encoding").is_none()
        && headers.count(b"Content-Length") <= 1
        && headers
            .first(b"Content-Length")
            .is_none_or(|v| wg::parse_u64(wg::trim_ascii(v)).is_some_and(|n| n <= MAX_REQUEST))
}

fn has_signature(body: &[u8]) -> bool {
    SIGNATURES.iter().any(|s| wg::contains_ci(body, s))
}

fn request_body(event: &v4::Event<'_>, flags: v4::Flags, out: &mut v4::Output<'_>) -> bool {
    let prev = event.state().unwrap_or(&[]);
    let body = event.blob();
    if prev.len() > TAIL || has_signature(body) {
        return false;
    }
    let mut stitch = [0; 2 * TAIL];
    let prefix = &body[..body.len().min(TAIL)];
    stitch[..prev.len()].copy_from_slice(prev);
    stitch[prev.len()..prev.len() + prefix.len()].copy_from_slice(prefix);
    let combined = &stitch[..prev.len() + prefix.len()];
    if !prev.is_empty() && has_signature(combined) {
        return false;
    }
    if flags.contains(v4::Flags::LAST) {
        return out.write(None, Some(&[]));
    }
    let tail = if body.len() >= TAIL { body } else { combined };
    out.write(None, Some(&tail[tail.len().saturating_sub(TAIL)..]))
}

// Opt in per response as well as in listener config. This is a small
// demonstration, not a JSON parser or a production data-redaction policy.
fn response_head(event: &v4::Event<'_>, out: &mut v4::Output<'_>) -> bool {
    let Some(line) = event.response_line() else {
        return false;
    };
    let headers = event.headers();
    let plain = headers.first(b"Content-Type").is_some_and(|v| {
        let media_type = v.split(|&b| b == b';').next().unwrap_or(&[]);
        wg::trim_ascii(media_type).eq_ignore_ascii_case(b"text/plain")
    });
    let identity = headers
        .first(b"Content-Encoding")
        .is_none_or(|v| wg::trim_ascii(v).eq_ignore_ascii_case(b"identity"));
    let selected = line.status == 200
        && plain
        && identity
        && headers.count(b"Content-Type") == 1
        && headers.count(b"Content-Encoding") <= 1
        && headers
            .first(b"X-WG-Mask-Digits")
            .is_some_and(|v| wg::trim_ascii(v) == b"yes");
    !selected || out.write(None, Some(MASK_STATE))
}

fn response_body(event: &v4::Event<'_>, out: &mut v4::Output<'_>) -> bool {
    let body = event.blob();
    if event.state() != Some(MASK_STATE) || !body.iter().any(u8::is_ascii_digit) {
        return true;
    }
    let mut buf = [0; REPLACE_CHUNK];
    let Some(replacement) = buf.get_mut(..body.len()) else {
        return false;
    };
    for (dest, &src) in replacement.iter_mut().zip(body) {
        *dest = if src.is_ascii_digit() { b'*' } else { src };
    }
    out.write(Some(replacement), None)
}
