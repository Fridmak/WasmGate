//! `redis_guard` — RESP protocol parsing and command policy in a WASM plugin.
//!
//! This is the nginx `stream` + `map`/ACL pattern applied to a binary protocol:
//! the plugin parses the first Redis command in every client chunk (both the
//! RESP array form and the inline form) and closes the connection when the
//! command is on the deny list.
//!
//! ```yaml
//! plugin:
//!   path: "plugins/examples/redis_guard/redis_guard.wasm"
//!   data_mode: "stream"
//! ```
//!
//! Redis has no request boundaries at the TCP level, so a single read may
//! contain half a command, several commands, or a command plus the start of the
//! next one. This plugin inspects the first *complete* command in the chunk and
//! fails open when the chunk does not contain one: stateless plugins cannot
//! buffer the remainder. Use the server's own Redis ACLs for enforcement.
//!
//! Verdicts in `stream` mode: 0 = forward, non-zero = close the connection.
//! Redis replies are passed unmodified.

#![no_std]

use wg_common::{self as wg, v4};

wg::wg_v4_plugin!(v4::CAP_OPAQUE, inspect);

/// Administrative/destructive commands. Everything not listed here is allowed,
/// which is the useful default for an application-facing cache endpoint.
const DENIED: &[&[u8]] = &[
    b"CONFIG",
    b"DEBUG",
    b"FLUSHALL",
    b"FLUSHDB",
    b"KEYS",
    b"MODULE",
    b"MONITOR",
    b"MIGRATE",
    b"MOVE",
    b"REPLICAOF",
    b"SLAVEOF",
    b"SAVE",
    b"BGSAVE",
    b"BGREWRITEAOF",
    b"SHUTDOWN",
    b"SWAPDB",
    b"RESTORE",
    b"ACL",
    b"CLUSTER",
    b"FAILOVER",
    b"PSYNC",
    b"SYNC",
    b"SCRIPT",
    b"EVAL",
    b"EVALSHA",
    b"EVAL_RO",
    b"EVALSHA_RO",
    b"FUNCTION",
];

/// Subcommands of otherwise harmless commands that are still dangerous.
const DENIED_SUBCOMMANDS: &[(&[u8], &[u8])] = &[
    (b"CLIENT", b"KILL"),
    (b"CLIENT", b"NO-EVICT"),
    (b"CLIENT", b"NO-TOUCH"),
    (b"COMMAND", b"DOCS"),
];

fn inspect(call: v4::Call, event: v4::Event<'_>, _: &mut v4::Output<'_>) -> i32 {
    if call.phase != v4::Phase::Opaque {
        return v4::DENY;
    }
    // Redis responses flow in the other direction and need no policy.
    if !call.flags.contains(v4::Flags::CLIENT_TO_SERVER) {
        return 0;
    }
    let Some((command, subcommand)) = first_command(event.blob()) else {
        // Partial or unparsable chunk: fail open, see the module docs.
        return 0;
    };
    if DENIED.iter().any(|d| wg::eq_ci(command, d)) {
        return 1;
    }
    if let Some(sub) = subcommand {
        if DENIED_SUBCOMMANDS
            .iter()
            .any(|(cmd, subcmd)| wg::eq_ci(command, cmd) && wg::eq_ci(sub, subcmd))
        {
            return 1;
        }
    }
    0
}

/// Returns the command name and, when present, the first argument.
///
/// RESP: `*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n`. Inline: `GET foo\r\n` (only used
/// by interactive clients and health probes, but worth covering).
fn first_command(buf: &[u8]) -> Option<(&[u8], Option<&[u8]>)> {
    if buf.first() == Some(&b'*') {
        parse_resp_array(buf)
    } else {
        parse_inline(buf)
    }
}

fn parse_resp_array(buf: &[u8]) -> Option<(&[u8], Option<&[u8]>)> {
    let mut pos = 1;
    let (count, next) = read_int(buf, pos)?;
    pos = next;
    if count == 0 || count > 1024 {
        return None;
    }
    // Terminator after the element count.
    pos = expect_crlf(buf, pos)?;

    let (command, next) = read_bulk(buf, pos)?;
    pos = next;
    if command.is_empty() {
        return None;
    }
    let sub = if count > 1 {
        read_bulk(buf, pos).map(|(arg, _)| arg)
    } else {
        None
    };
    Some((command, sub))
}

fn read_bulk(buf: &[u8], pos: usize) -> Option<(&[u8], usize)> {
    if *buf.get(pos)? != b'$' {
        return None;
    }
    let (len, next) = read_int(buf, pos + 1)?;
    let next = expect_crlf(buf, next)?;
    let len = usize::try_from(len).ok()?;
    if len > 64 * 1024 {
        return None;
    }
    let end = next.checked_add(len)?;
    let value = buf.get(next..end)?;
    // The trailing CRLF must also be present for the command to be complete.
    let after = expect_crlf(buf, end)?;
    Some((value, after))
}

fn read_int(buf: &[u8], mut pos: usize) -> Option<(i64, usize)> {
    let mut value: i64 = 0;
    let mut digits = 0;
    while let Some(&c) = buf.get(pos) {
        if c == b'\r' {
            break;
        }
        if !c.is_ascii_digit() {
            return None;
        }
        value = value.checked_mul(10)?.checked_add((c - b'0') as i64)?;
        digits += 1;
        if digits > 9 {
            return None;
        }
        pos += 1;
    }
    if digits == 0 {
        return None;
    }
    Some((value, pos))
}

fn expect_crlf(buf: &[u8], pos: usize) -> Option<usize> {
    if buf.get(pos) == Some(&b'\r') && buf.get(pos + 1) == Some(&b'\n') {
        Some(pos + 2)
    } else {
        None
    }
}

fn parse_inline(buf: &[u8]) -> Option<(&[u8], Option<&[u8]>)> {
    let end = wg::find(buf, b"\n")?;
    let line = wg::trim_ascii(&buf[..end]);
    let mut parts = line
        .split(|&b| b == b' ' || b == b'\t')
        .filter(|p| !p.is_empty());
    let command = parts.next()?;
    let sub = parts.next();
    Some((command, sub))
}
