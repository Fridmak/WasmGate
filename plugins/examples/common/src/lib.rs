//! Shared, dependency-free helpers for the WasmGate example plugins.
//!
//! Every plugin is a `#![no_std]` cdylib with no WASI imports. This crate is
//! linked into each plugin; it performs no I/O and allocates nothing.
//!
//! The host copies an `on_data` chunk into the plugin's 32 KiB scratch region
//! before every hook call and copies it back afterwards, so the helpers below
//! operate directly on that buffer. In-place mutation is limited to the chunk:
//! bytes cannot be inserted or removed.

#![no_std]

/// The host guarantees a 32 KiB scratch region behind `on_data_ptr()`.
pub const SCRATCH_CAP: usize = 32 * 1024;

/// The host guarantees a 4 KiB state scratch region behind `on_state_ptr()`
/// for ABI v3 (`on_data_state`) plugins.
pub const STATE_CAP: usize = 4 * 1024;

static mut SCRATCH: [u8; SCRATCH_CAP] = [0; SCRATCH_CAP];
static mut STATE_SCRATCH: [u8; STATE_CAP] = [0; STATE_CAP];

/// Returns the guest address of the 32 KiB `on_data` scratch buffer.
pub fn scratch_ptr() -> i32 {
    // `addr_of_mut!` avoids creating a reference to `static mut`.
    core::ptr::addr_of_mut!(SCRATCH) as i32
}

/// Returns the guest address of the 4 KiB `on_data_state` scratch buffer.
pub fn state_ptr() -> i32 {
    core::ptr::addr_of_mut!(STATE_SCRATCH) as i32
}

/// Packs a 32-bit verdict and the updated state byte length into the `i64`
/// return value of `on_data_state`.
pub const fn pack_state_verdict(verdict: i32, state_len: usize) -> i64 {
    (((state_len as u32 as u64) << 32) | (verdict as u32 as u64)) as i64
}

/// Validates a host-provided `(state_ptr, state_cap)` pair and returns the
/// mutable state buffer slice.
///
/// # Safety
/// The caller must ensure the region is valid for the duration of the call.
pub unsafe fn state_mut<'a>(ptr: i32, cap: i32) -> Option<&'a mut [u8]> {
    if ptr < 0 || cap < 0 {
        return None;
    }
    let n = cap as usize;
    if n > STATE_CAP {
        return None;
    }
    Some(core::slice::from_raw_parts_mut(ptr as usize as *mut u8, n))
}

/// Validates a host-provided (ptr, len) pair and returns a shared slice.
///
/// # Safety
/// The caller must ensure the region is valid for the duration of the call.
/// The host owns and passes the scratch buffer, so this holds for hooks.
pub unsafe fn payload<'a>(ptr: i32, len: i32) -> Option<&'a [u8]> {
    let (p, n) = validate(ptr, len)?;
    Some(core::slice::from_raw_parts(p, n))
}

/// Mutable variant used by hooks that rewrite bytes in place.
///
/// # Safety
/// Same contract as [`payload`]. The host invokes at most one hook per instance
/// at a time, so this slice is not aliased by another guest call.
pub unsafe fn payload_mut<'a>(ptr: i32, len: i32) -> Option<&'a mut [u8]> {
    let (p, n) = validate(ptr, len)?;
    Some(core::slice::from_raw_parts_mut(p as *mut u8, n))
}

fn validate(ptr: i32, len: i32) -> Option<(*const u8, usize)> {
    if ptr < 0 || len < 0 {
        return None;
    }
    let n = len as usize;
    if n > SCRATCH_CAP {
        return None;
    }
    Some((ptr as usize as *const u8, n))
}

pub fn lower(c: u8) -> u8 {
    if c.is_ascii_uppercase() {
        c + 32
    } else {
        c
    }
}

pub fn eq_ci(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b.iter()).all(|(x, y)| lower(*x) == lower(*y))
}

pub fn starts_with_ci(hay: &[u8], prefix: &[u8]) -> bool {
    hay.len() >= prefix.len() && eq_ci(&hay[..prefix.len()], prefix)
}

pub fn ends_with_ci(hay: &[u8], suffix: &[u8]) -> bool {
    hay.len() >= suffix.len() && eq_ci(&hay[hay.len() - suffix.len()..], suffix)
}

pub fn find(hay: &[u8], needle: &[u8]) -> Option<usize> {
    if needle.is_empty() {
        return Some(0);
    }
    if needle.len() > hay.len() {
        return None;
    }
    let mut i = 0;
    while i + needle.len() <= hay.len() {
        if &hay[i..i + needle.len()] == needle {
            return Some(i);
        }
        i += 1;
    }
    None
}

pub fn find_ci(hay: &[u8], needle: &[u8]) -> Option<usize> {
    if needle.is_empty() {
        return Some(0);
    }
    if needle.len() > hay.len() {
        return None;
    }
    let mut i = 0;
    while i + needle.len() <= hay.len() {
        if eq_ci(&hay[i..i + needle.len()], needle) {
            return Some(i);
        }
        i += 1;
    }
    None
}

pub fn contains(hay: &[u8], needle: &[u8]) -> bool {
    find(hay, needle).is_some()
}

pub fn contains_ci(hay: &[u8], needle: &[u8]) -> bool {
    find_ci(hay, needle).is_some()
}

pub fn trim_ascii(mut b: &[u8]) -> &[u8] {
    while let Some(&c) = b.first() {
        if c == b' ' || c == b'\t' || c == b'\r' || c == b'\n' {
            b = &b[1..];
        } else {
            break;
        }
    }
    while let Some(&c) = b.last() {
        if c == b' ' || c == b'\t' || c == b'\r' || c == b'\n' {
            b = &b[..b.len() - 1];
        } else {
            break;
        }
    }
    b
}

pub fn be16(b: &[u8]) -> Option<u16> {
    Some(((*b.get(0)? as u16) << 8) | (*b.get(1)? as u16))
}

pub fn be24(b: &[u8]) -> Option<usize> {
    Some(
        ((*b.get(0)? as usize) << 16)
            | ((*b.get(1)? as usize) << 8)
            | (*b.get(2)? as usize),
    )
}

pub fn be32(b: &[u8]) -> Option<u32> {
    Some(
        ((*b.get(0)? as u32) << 24)
            | ((*b.get(1)? as u32) << 16)
            | ((*b.get(2)? as u32) << 8)
            | (*b.get(3)? as u32),
    )
}

pub fn parse_u16(b: &[u8]) -> Option<u16> {
    let mut v: u16 = 0;
    if b.is_empty() {
        return None;
    }
    for &c in b {
        if !c.is_ascii_digit() {
            return None;
        }
        v = v.checked_mul(10)?.checked_add((c - b'0') as u16)?;
    }
    Some(v)
}

pub fn parse_u64(b: &[u8]) -> Option<u64> {
    let mut v: u64 = 0;
    if b.is_empty() {
        return None;
    }
    for &c in b {
        if !c.is_ascii_digit() {
            return None;
        }
        v = v.checked_mul(10)?.checked_add((c - b'0') as u64)?;
    }
    Some(v)
}

/// A parsed HTTP/1.x request line plus the raw header block.
pub struct Request<'a> {
    pub method: &'a [u8],
    pub target: &'a [u8],
    pub version: &'a [u8],
    pub headers: &'a [u8],
}

/// Parses the request line and returns the rest as raw headers.
///
/// Returns `None` when the first line is incomplete: a stateless plugin cannot
/// tell a partial request from a non-HTTP protocol, so callers should fail open
/// (allow) in that case. See the README: strict header policy needs ABI v3
/// per-connection state.
pub fn parse_request(payload: &[u8]) -> Option<Request<'_>> {
    let line_end = match find(payload, b"\r\n") {
        Some(p) => p,
        None => find(payload, b"\n")?,
    };
    let line = &payload[..line_end];
    let mut parts = line.split(|&b| b == b' ');
    let method = parts.next()?;
    let target = parts.next()?;
    let version = parts.next().unwrap_or(&[]);
    if method.is_empty() || target.is_empty() {
        return None;
    }
    let skip = if payload.get(line_end) == Some(&b'\r')
        && payload.get(line_end + 1) == Some(&b'\n')
    {
        2
    } else {
        1
    };
    Some(Request {
        method,
        target,
        version,
        headers: &payload[line_end + skip..],
    })
}

/// Returns the first value of a header, with the name matched case-insensitively.
pub fn header_value<'a>(headers: &'a [u8], name: &[u8]) -> Option<&'a [u8]> {
    for line in headers.split(|&b| b == b'\n') {
        let line = trim_ascii(line);
        if line.is_empty() {
            break;
        }
        if let Some(colon) = line.iter().position(|&b| b == b':') {
            if eq_ci(trim_ascii(&line[..colon]), name) {
                return Some(trim_ascii(&line[colon + 1..]));
            }
        }
    }
    None
}

/// Counts header occurrences, so duplicate `Content-Length` is detectable.
pub fn header_count(headers: &[u8], name: &[u8]) -> usize {
    let mut n = 0;
    for line in headers.split(|&b| b == b'\n') {
        let line = trim_ascii(line);
        if line.is_empty() {
            break;
        }
        if let Some(colon) = line.iter().position(|&b| b == b':') {
            if eq_ci(trim_ascii(&line[..colon]), name) {
                n += 1;
            }
        }
    }
    n
}

/// Rewrites a header value in place, padding or truncating to the existing
/// value length. Returns true when the header was found.
pub fn mask_header(buf: &mut [u8], name: &[u8], replacement: &[u8]) -> bool {
    let mut i = 0;
    while i < buf.len() {
        let end = match find(&buf[i..], b"\n") {
            Some(p) => i + p,
            None => buf.len(),
        };
        let line = &buf[i..end];
        if line.is_empty() || (line.len() == 1 && line[0] == b'\r') {
            return false;
        }
        if let Some(colon) = line.iter().position(|&b| b == b':') {
            if eq_ci(trim_ascii(&line[..colon]), name) {
                let mut vend = end;
                if vend > i + colon + 1 && buf[vend - 1] == b'\r' {
                    vend -= 1;
                }
                let mut vstart = i + colon + 1;
                while vstart < vend && (buf[vstart] == b' ' || buf[vstart] == b'\t') {
                    vstart += 1;
                }
                for k in 0..vend - vstart {
                    buf[vstart + k] = if k < replacement.len() {
                        replacement[k]
                    } else {
                        b' '
                    };
                }
                return true;
            }
        }
        i = end + 1;
    }
    false
}

/// Parses the status code of an HTTP/1.x response.
pub fn parse_status(payload: &[u8]) -> Option<u16> {
    let line_end = match find(payload, b"\r\n") {
        Some(p) => p,
        None => find(payload, b"\n")?,
    };
    let mut parts = payload[..line_end].split(|&b| b == b' ');
    let version = parts.next()?;
    if !version.starts_with(b"HTTP/") {
        return None;
    }
    let code = parts.next()?;
    if code.len() != 3 {
        return None;
    }
    parse_u16(code)
}

pub fn is_http_method(m: &[u8]) -> bool {
    m == b"GET"
        || m == b"HEAD"
        || m == b"POST"
        || m == b"PUT"
        || m == b"PATCH"
        || m == b"DELETE"
        || m == b"OPTIONS"
        || m == b"TRACE"
        || m == b"CONNECT"
}

pub fn looks_like_http(payload: &[u8]) -> bool {
    const METHODS: &[&[u8]] = &[
        b"GET",
        b"HEAD",
        b"POST",
        b"PUT",
        b"PATCH",
        b"DELETE",
        b"OPTIONS",
        b"TRACE",
        b"CONNECT",
    ];
    for m in METHODS {
        if payload.starts_with(m) && payload.get(m.len()) == Some(&b' ') {
            return true;
        }
    }
    false
}

/// Extracts the SNI host name from one TLS ClientHello record.
///
/// This is the same wire format the Go `internal/tlspeek` package parses. It
/// performs no allocation and rejects malformed or truncated input.
pub fn parse_sni(rec: &[u8]) -> Option<&[u8]> {
    if rec.len() < 5 || rec[0] != 0x16 || rec[1] != 0x03 {
        return None;
    }
    let rec_len = be16(&rec[3..5])? as usize;
    let body = rec.get(5..5 + rec_len)?;
    if body.len() < 4 || body[0] != 0x01 {
        return None;
    }
    let ch_len = be24(&body[1..4])?;
    let ch = body.get(4..4 + ch_len)?;

    // ClientVersion (2) + Random (32) + SessionID length (1).
    if ch.len() < 35 {
        return None;
    }
    let sid_len = ch[34] as usize;
    let ch = ch.get(35 + sid_len..)?;

    let cs_len = be16(ch.get(0..2)?)? as usize;
    let ch = ch.get(2 + cs_len..)?;

    let comp_len = *ch.get(0)? as usize;
    let ch = ch.get(1 + comp_len..)?;

    let ext_total = be16(ch.get(0..2)?)? as usize;
    let mut exts = ch.get(2..2 + ext_total)?;

    while exts.len() >= 4 {
        let ext_type = be16(&exts[0..2])?;
        let ext_len = be16(&exts[2..4])? as usize;
        let data = exts.get(4..4 + ext_len)?;
        exts = &exts[4 + ext_len..];
        if ext_type == 0x0000 {
            return parse_server_name(data);
        }
    }
    None
}

fn parse_server_name(b: &[u8]) -> Option<&[u8]> {
    let list_len = be16(b.get(0..2)?)? as usize;
    let mut list = b.get(2..2 + list_len)?;
    while list.len() >= 3 {
        let name_type = list[0];
        let name_len = be16(&list[1..3])? as usize;
        let name = list.get(3..3 + name_len)?;
        list = &list[3 + name_len..];
        if name_type == 0 && valid_hostname(name) {
            return Some(name);
        }
    }
    None
}

fn valid_hostname(b: &[u8]) -> bool {
    if b.is_empty() || b[0] == b'.' || b[b.len() - 1] == b'.' {
        return false;
    }
    for &c in b {
        if !(c.is_ascii_alphanumeric() || c == b'-' || c == b'.') {
            return false;
        }
    }
    true
}

/// FNV-1a 32-bit, useful for inexpensive fingerprints.
pub fn fnv1a32(data: &[u8]) -> u32 {
    let mut h: u32 = 0x811c9dc5;
    for &b in data {
        h ^= b as u32;
        h = h.wrapping_mul(0x01000193);
    }
    h
}
