//! Small no-std helpers for ABI v4 example plugins.
//!
//! The host owns the envelope layout and passes its address/length to
//! `on_event`. This module provides bounded readers plus the fixed guest
//! windows used by simple examples. It intentionally does not parse HTTP.

#![allow(dead_code)]

pub const ABI: i32 = 0x0400;
pub const CAP_BLOB: i64 = 1 << 0;
pub const CAP_HEADER_INDEX: i64 = 1 << 1;
pub const CAP_START_LINE: i64 = 1 << 2;
pub const CAP_STATE_IN: i64 = 1 << 4;
pub const CAP_STATE_OUT: i64 = 1 << 5;
pub const CAP_OPAQUE: i64 = 1 << 6;
pub const CAP_MUTATION: i64 = 1 << 7;

pub const PHASE_REQUEST_HEADERS: i32 = 1;
pub const PHASE_REQUEST_BODY: i32 = 2;
pub const PHASE_RESPONSE_HEADERS: i32 = 3;
pub const PHASE_RESPONSE_BODY: i32 = 4;
pub const PHASE_OPAQUE: i32 = 5;

pub const FLAG_FIRST: i32 = 1;
pub const FLAG_LAST: i32 = 2;
pub const FLAG_TRUNCATED: i32 = 4;
pub const FLAG_CLIENT_TO_SERVER: i32 = 8;
pub const FLAG_SERVER_TO_CLIENT: i32 = 16;

const MAGIC_IN: u32 = 0x5747_3445;
const MAGIC_OUT: u32 = 0x5747_344f;
const HDR_LEN: usize = 24;
const SEC_STRIDE: usize = 16;
const SEC_BLOB: u16 = 1;
const SEC_INDEX: u16 = 2;
const SEC_STATE: u16 = 5;
const SEC_MUTATION: u16 = 6;
const WINDOW_CAP: usize = 48 * 1024;
const OUT_CAP: usize = 4 * 1024;

static mut WINDOW: [u8; WINDOW_CAP] = [0; WINDOW_CAP];
static mut OUT: [u8; OUT_CAP] = [0; OUT_CAP];

pub fn buf_ptr() -> i32 {
    core::ptr::addr_of_mut!(WINDOW) as i32
}

pub const fn buf_len() -> i32 {
    WINDOW_CAP as i32
}

pub fn out_ptr() -> i32 {
    core::ptr::addr_of_mut!(OUT) as i32
}

pub const fn out_len() -> i32 {
    OUT_CAP as i32
}

fn u16_at(b: &[u8], off: usize) -> Option<u16> {
    Some(u16::from_le_bytes([*b.get(off)?, *b.get(off + 1)?]))
}

fn u32_at(b: &[u8], off: usize) -> Option<u32> {
    Some(u32::from_le_bytes([
        *b.get(off)?,
        *b.get(off + 1)?,
        *b.get(off + 2)?,
        *b.get(off + 3)?,
    ]))
}

/// A zero-copy view of a validated input envelope.
pub struct Event<'a> {
    raw: &'a [u8],
    arena: usize,
    arena_len: usize,
    sections: usize,
}

impl<'a> Event<'a> {
    /// # Safety
    /// `ptr..ptr+len` must point to guest-readable memory for the duration of
    /// the call. The host supplies this region.
    pub unsafe fn from_raw(ptr: i32, len: i32) -> Option<Event<'a>> {
        if ptr < 0 || len < 0 {
            return None;
        }
        let raw = core::slice::from_raw_parts(ptr as usize as *const u8, len as usize);
        if raw.len() < HDR_LEN || u32_at(raw, 0)? != MAGIC_IN || u16_at(raw, 6)? as usize != HDR_LEN {
            return None;
        }
        let total = u32_at(raw, 8)? as usize;
        let sections = u16_at(raw, 12)? as usize;
        let arena = u32_at(raw, 16)? as usize;
        let arena_len = u32_at(raw, 20)? as usize;
        if total > raw.len() || total < HDR_LEN || sections == 0 || sections > 8 ||
            HDR_LEN + sections * SEC_STRIDE > arena || arena + arena_len > total {
            return None;
        }
        Some(Event { raw: &raw[..total], arena, arena_len, sections })
    }

    pub fn phase_blob(&self) -> Option<&'a [u8]> {
        self.section(SEC_BLOB).and_then(|(off, len, _)| self.view(off, len))
    }

    pub fn state(&self) -> Option<&'a [u8]> {
        self.section(SEC_STATE).and_then(|(off, len, _)| self.view(off, len))
    }

    pub fn header_index(&self) -> Option<HeaderIndex<'a>> {
        self.section(SEC_INDEX).map(|(off, len, count)| HeaderIndex {
            raw: self.raw,
            off,
            len,
            count,
            blob: self.phase_blob(),
        })
    }

    fn section(&self, wanted: u16) -> Option<(usize, usize, usize)> {
        for i in 0..self.sections {
            let off = HDR_LEN + i * SEC_STRIDE;
            if u16_at(self.raw, off)? == wanted {
                return Some((
                    u32_at(self.raw, off + 4)? as usize,
                    u32_at(self.raw, off + 8)? as usize,
                    u32_at(self.raw, off + 12)? as usize,
                ));
            }
        }
        None
    }

    fn view(&self, off: usize, len: usize) -> Option<&'a [u8]> {
        if off < self.arena || off.checked_add(len)? > self.arena.checked_add(self.arena_len)? {
            return None;
        }
        self.raw.get(off..off + len)
    }
}

pub struct HeaderIndex<'a> {
    raw: &'a [u8],
    off: usize,
    len: usize,
    count: usize,
    blob: Option<&'a [u8]>,
}

impl<'a> HeaderIndex<'a> {
    pub fn get(&self, n: usize) -> Option<(&'a [u8], &'a [u8])> {
        if n >= self.count || n.checked_mul(16)? + 16 > self.len {
            return None;
        }
        let e = self.off + n * 16;
        let name_off = u32_at(self.raw, e)? as usize;
        let val_off = u32_at(self.raw, e + 4)? as usize;
        let name_len = u16_at(self.raw, e + 8)? as usize;
        let val_len = u16_at(self.raw, e + 10)? as usize;
        let blob = self.blob?;
        let name = blob.get(name_off..name_off + name_len)?;
        let value = blob.get(val_off..val_off + val_len)?;
        Some((name, value))
    }
}

/// Clears and writes an OUT envelope containing optional same-length mutation
/// and next-state bytes. Insertion/deletion is intentionally not supported by
/// the first SDK surface.
pub fn write_output(mutation: Option<&[u8]>, state: Option<&[u8]>) -> bool {
    let mutation = mutation.unwrap_or(&[]);
    let state = state.unwrap_or(&[]);
    if mutation.len() > OUT_CAP || state.len() > OUT_CAP || mutation.len() + state.len() > OUT_CAP - 64 {
        return false;
    }
    unsafe { core::ptr::write_bytes(OUT.as_mut_ptr(), 0, OUT_CAP) };
    let mut sections = 0usize;
    if state.len() > 0 { sections += 1; }
    if mutation.len() > 0 { sections += 1; }
    let table_end = HDR_LEN + sections * SEC_STRIDE;
    let arena = (table_end + 7) & !7;
    let state_off = arena;
    let mutation_off = (arena + state.len() + 3) & !3;
    let total = (mutation_off + mutation.len() + 7) & !7;
    if total > OUT_CAP { return false; }
    put32(0, MAGIC_OUT);
    put16(4, 1);
    put16(6, HDR_LEN as u16);
    put32(8, total as u32);
    put16(12, sections as u16);
    put16(14, SEC_STRIDE as u16);
    put32(16, arena as u32);
    put32(20, (mutation_off + mutation.len() - arena) as u32);
    let mut sec = 0usize;
    if !state.is_empty() {
        put_section(sec, SEC_STATE, state_off, state.len()); sec += 1;
        unsafe { OUT[state_off..state_off + state.len()].copy_from_slice(state); }
    }
    if !mutation.is_empty() {
        put_section(sec, SEC_MUTATION, mutation_off, mutation.len());
        unsafe { OUT[mutation_off..mutation_off + mutation.len()].copy_from_slice(mutation); }
    }
    true
}

fn put_section(n: usize, typ: u16, off: usize, len: usize) {
    let base = HDR_LEN + n * SEC_STRIDE;
    put16(base, typ); put32(base + 4, off as u32); put32(base + 8, len as u32);
}

fn put16(off: usize, v: u16) { unsafe { OUT[off..off + 2].copy_from_slice(&v.to_le_bytes()); } }
fn put32(off: usize, v: u32) { unsafe { OUT[off..off + 4].copy_from_slice(&v.to_le_bytes()); } }
