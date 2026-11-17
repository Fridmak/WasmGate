//! `http_guard` inspects an indexed HTTP/1 request head (`data_mode: headers`).
//! The built-in credentials are for the demo only, not production use.

#![no_std]

use wg_common::{self as wg, v4};

wg::wg_v4_plugin!(v4::CAP_HTTP_HEADERS, inspect);

const ALLOWED_METHODS: &[&[u8]] = &[
    b"GET", b"HEAD", b"POST", b"PUT", b"PATCH", b"DELETE", b"OPTIONS",
];
const BLOCKED_PREFIXES: &[&[u8]] = &[
    b"/admin",
    b"/.git",
    b"/.env",
    b"/wp-login",
    b"/wp-admin",
    b"/phpmyadmin",
    b"/actuator",
    b"/server-status",
];
const BLOCKED_UA: &[&[u8]] = &[
    b"sqlmap", b"nikto", b"nmap", b"masscan", b"zgrab", b"nuclei",
];
// Basic user / pass. Do not use this as an authentication boundary.
const DEMO_AUTH: &[u8] = b"dXNlcjpwYXNz";

fn inspect(call: v4::Call, event: v4::Event<'_>, _: &mut v4::Output<'_>) -> i32 {
    if call.phase != v4::Phase::RequestHeaders {
        return v4::DENY;
    }
    let Some(req) = event.request_line() else {
        return v4::DENY;
    };
    let headers = event.headers();
    if !ALLOWED_METHODS.contains(&req.method) || bad_target(req.target) {
        return v4::DENY;
    }
    if headers.iter().count() > 100
        || headers
            .iter()
            .any(|(name, value)| name.len() + value.len() > 8192)
    {
        return v4::DENY;
    }
    match headers.first(b"Host").map(wg::trim_ascii) {
        Some(host) if host.is_empty() || host.len() > 255 || wg::contains(host, b"..") => {
            return v4::DENY
        }
        None if req.version == b"HTTP/1.1" => return v4::DENY,
        _ => {}
    }
    let cl = headers.count(b"Content-Length");
    if cl > 1 || (cl != 0 && headers.first(b"Transfer-Encoding").is_some()) {
        return v4::DENY;
    }
    if headers.iter().any(|(name, value)| {
        name.eq_ignore_ascii_case(b"User-Agent")
            && BLOCKED_UA.iter().any(|bad| wg::contains_ci(value, bad))
    }) {
        return v4::DENY;
    }
    if headers.count(b"Authorization") != 1 {
        return v4::DENY;
    }
    let auth = wg::trim_ascii(headers.first(b"Authorization").unwrap_or(&[]));
    if wg::starts_with_ci(auth, b"Basic ") && auth[6..] == *DEMO_AUTH {
        v4::ALLOW
    } else {
        v4::DENY
    }
}

fn bad_target(target: &[u8]) -> bool {
    target.len() > 8192
        || wg::contains(target, b"../")
        || wg::contains(target, b"..\\")
        || wg::contains_ci(target, b"%2e%2e")
        || wg::contains_ci(target, b"%2f..")
        || wg::contains_ci(target, b"%5c..")
        || wg::contains(target, b"//")
        || BLOCKED_PREFIXES
            .iter()
            .any(|p| wg::starts_with_ci(target, p))
}
