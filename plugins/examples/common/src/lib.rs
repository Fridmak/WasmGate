//! No-std WasmGate ABI v4 SDK and byte helpers for the reference plugins.
//! HTTP guests use the host's typed header index, not another HTTP parser.

#![no_std]

pub mod v4;

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
    Some(((*b.first()? as u16) << 8) | (*b.get(1)? as u16))
}

pub fn be24(b: &[u8]) -> Option<usize> {
    Some(((*b.first()? as usize) << 16) | ((*b.get(1)? as usize) << 8) | (*b.get(2)? as usize))
}

pub fn be32(b: &[u8]) -> Option<u32> {
    Some(
        ((*b.first()? as u32) << 24)
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

/// HTTP methods recognised by [`is_http_method`] and [`looks_like_http`].
/// Policy plugins may allow a narrower subset.
pub const HTTP_METHODS: &[&[u8]] = &[
    b"GET", b"HEAD", b"POST", b"PUT", b"PATCH", b"DELETE", b"OPTIONS", b"TRACE", b"CONNECT",
];

pub fn is_http_method(m: &[u8]) -> bool {
    HTTP_METHODS.iter().any(|candidate| *candidate == m)
}

pub fn looks_like_http(payload: &[u8]) -> bool {
    HTTP_METHODS
        .iter()
        .any(|method| payload.starts_with(method) && payload.get(method.len()) == Some(&b' '))
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

    let comp_len = *ch.first()? as usize;
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
