//! ABI v4 envelope codec and export glue for no-std guests.
//!
//! Offsets are absolute from the start of the envelope. IN views borrow the
//! host-written bytes for one call only; effects are serialized separately.

pub const ABI: i32 = 0x0400;
pub const CAP_BLOB: i64 = 1 << 0;
pub const CAP_HEADER_INDEX: i64 = 1 << 1;
pub const CAP_START_LINE: i64 = 1 << 2;
pub const CAP_CONN_INFO: i64 = 1 << 3;
pub const CAP_STATE_IN: i64 = 1 << 4;
pub const CAP_STATE_OUT: i64 = 1 << 5;
pub const CAP_OPAQUE: i64 = 1 << 6;
pub const CAP_MUTATION: i64 = 1 << 7;
pub const CAP_LIFECYCLE: i64 = 1 << 8;
pub const CAP_HTTP_HEADERS: i64 = CAP_HEADER_INDEX | CAP_START_LINE;

pub const ALLOW: i32 = 0;
pub const DENY: i32 = 1;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(i32)]
pub enum Phase {
    RequestHeaders = 1,
    RequestBody = 2,
    ResponseHeaders = 3,
    ResponseBody = 4,
    Opaque = 5,
}

impl TryFrom<i32> for Phase {
    type Error = ();

    fn try_from(raw: i32) -> Result<Self, Self::Error> {
        match raw {
            1 => Ok(Self::RequestHeaders),
            2 => Ok(Self::RequestBody),
            3 => Ok(Self::ResponseHeaders),
            4 => Ok(Self::ResponseBody),
            5 => Ok(Self::Opaque),
            _ => Err(()),
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub struct Flags(u32);

impl Flags {
    pub const FIRST: Self = Self(1);
    pub const LAST: Self = Self(2);
    pub const TRUNCATED: Self = Self(4);
    pub const CLIENT_TO_SERVER: Self = Self(8);
    pub const SERVER_TO_CLIENT: Self = Self(16);

    pub fn contains(self, flag: Self) -> bool {
        self.0 & flag.0 == flag.0
    }
}

#[derive(Clone, Copy, Debug)]
pub struct Call {
    pub conn_id: u64,
    pub phase: Phase,
    pub flags: Flags,
}

const MAGIC_IN: u32 = 0x5747_3445;
const MAGIC_OUT: u32 = 0x5747_344f;
const HDR_LEN: usize = 24;
const SEC_STRIDE: usize = 16;
const MAX_SECTIONS: usize = 8;
const MAX_ENTRIES: usize = 8192;
const MAX_PAYLOAD: usize = 1 << 30;
const ENTRY_LEN: usize = 16;
const START_LEN: usize = 24;
const CONN_LEN: usize = 80;
const SEC_BLOB: u16 = 1;
const SEC_INDEX: u16 = 2;
const SEC_START: u16 = 3;
const SEC_CONN: u16 = 4;
const SEC_STATE: u16 = 5;
const SEC_MUTATION: u16 = 6;

fn view(raw: &[u8], offset: usize, len: usize) -> Option<&[u8]> {
    raw.get(offset..offset.checked_add(len)?)
}

fn u16_at(raw: &[u8], offset: usize) -> Option<u16> {
    Some(u16::from_le_bytes(view(raw, offset, 2)?.try_into().ok()?))
}

fn u32_at(raw: &[u8], offset: usize) -> Option<usize> {
    Some(u32::from_le_bytes(view(raw, offset, 4)?.try_into().ok()?) as usize)
}

#[derive(Clone, Copy, Default)]
struct Section {
    kind: u16,
    offset: usize,
    len: usize,
    count: usize,
}

struct Envelope<'a> {
    raw: &'a [u8],
    arena: core::ops::Range<usize>,
    sections: [Section; MAX_SECTIONS],
    count: usize,
}

impl<'a> Envelope<'a> {
    fn parse(raw: &'a [u8], magic: u32) -> Option<Self> {
        if u32_at(raw, 0)? != magic as usize
            || u16_at(raw, 4)? != 1
            || u16_at(raw, 6)? as usize != HDR_LEN
            || u16_at(raw, 14)? as usize != SEC_STRIDE
        {
            return None;
        }
        let total = u32_at(raw, 8)?;
        let count = u16_at(raw, 12)? as usize;
        let base = u32_at(raw, 16)?;
        let arena_end = base.checked_add(u32_at(raw, 20)?)?;
        let table_end = HDR_LEN + count * SEC_STRIDE;
        if count > MAX_SECTIONS
            || total % 8 != 0
            || base % 8 != 0
            || table_end > base
            || arena_end > total
        {
            return None;
        }
        let raw = raw.get(..total)?;
        let mut sections = [Section::default(); MAX_SECTIONS];
        let mut prev = 0;
        for i in 0..count {
            let off = HDR_LEN + i * SEC_STRIDE;
            let entry = Section {
                kind: u16_at(raw, off)?,
                offset: u32_at(raw, off + 4)?,
                len: u32_at(raw, off + 8)?,
                count: u32_at(raw, off + 12)?,
            };
            let end = entry.offset.checked_add(entry.len)?;
            if entry.kind <= prev
                || u16_at(raw, off + 2)? != 0
                || entry.len > MAX_PAYLOAD
                || entry.offset < table_end
                || entry.offset % 4 != 0
                || end > total
                || sections[..i].iter().any(|other| {
                    entry.len != 0
                        && other.len != 0
                        && other.offset < end
                        && entry.offset < other.offset + other.len
                })
            {
                return None;
            }
            sections[i] = entry;
            prev = entry.kind;
        }
        Some(Self {
            raw,
            arena: base..arena_end,
            sections,
            count,
        })
    }

    fn section_bytes(&self, s: Section) -> &'a [u8] {
        &self.raw[s.offset..s.offset + s.len]
    }
}

#[derive(Clone, Copy)]
struct Blob<'a> {
    base: usize,
    bytes: &'a [u8],
}

impl<'a> Blob<'a> {
    fn get(self, offset: usize, len: usize) -> Option<&'a [u8]> {
        view(self.bytes, offset.checked_sub(self.base)?, len)
    }

    fn optional(self, offset: usize, len: usize) -> Option<&'a [u8]> {
        if len == 0 {
            Some(&[])
        } else {
            self.get(offset, len)
        }
    }
}

/// Validated, zero-copy IN views. A missing state section is distinct from
/// a present, zero-byte section.
pub struct Event<'a> {
    blob: Blob<'a>,
    state: Option<&'a [u8]>,
    headers: HeaderIndex<'a>,
    start: Option<StartLine<'a>>,
}

impl<'a> Event<'a> {
    pub fn parse(raw: &'a [u8]) -> Option<Self> {
        let env = Envelope::parse(raw, MAGIC_IN)?;
        let s = env.sections[0];
        if s.kind != SEC_BLOB
            || s.count != 0
            || s.offset != env.arena.start
            || s.offset + s.len > env.arena.end
        {
            return None;
        }
        let blob = Blob {
            base: s.offset,
            bytes: env.section_bytes(s),
        };
        let mut event = Self {
            blob,
            state: None,
            headers: HeaderIndex { bytes: &[], blob },
            start: None,
        };
        for &s in &env.sections[1..env.count] {
            let data = env.section_bytes(s);
            match s.kind {
                SEC_STATE => {
                    if s.count != 0
                        || s.offset < blob.base + blob.bytes.len()
                        || s.offset + s.len > env.arena.end
                    {
                        return None;
                    }
                    event.state = Some(data);
                }
                SEC_INDEX => {
                    if s.offset < env.arena.end
                        || s.count == 0
                        || s.count > MAX_ENTRIES
                        || s.len != s.count * ENTRY_LEN
                    {
                        return None;
                    }
                    for entry in data.chunks_exact(ENTRY_LEN) {
                        entry_fields(entry, blob)?;
                    }
                    event.headers.bytes = data;
                }
                SEC_START => {
                    if s.offset < env.arena.end || s.count != 0 || s.len != START_LEN {
                        return None;
                    }
                    let field =
                        |off, len| blob.optional(u32_at(data, off)?, u16_at(data, len)? as usize);
                    event.start = Some(StartLine {
                        method_or_reason: field(0, 12)?,
                        path_or_status: field(4, 14)?,
                        version: field(8, 16)?,
                    });
                }
                SEC_CONN => {
                    if s.offset < env.arena.end || s.count != 0 || s.len != CONN_LEN {
                        return None;
                    }
                    blob.optional(u32_at(data, 48)?, u32_at(data, 52)?)?;
                    blob.optional(u32_at(data, 56)?, u32_at(data, 60)?)?;
                }
                _ => {} // future IN section types, already checked structurally
            }
        }
        Some(event)
    }

    pub fn blob(&self) -> &'a [u8] {
        self.blob.bytes
    }

    pub fn state(&self) -> Option<&'a [u8]> {
        self.state
    }

    pub fn headers(&self) -> HeaderIndex<'a> {
        self.headers
    }

    /// Available on `Phase::RequestHeaders`.
    pub fn request_line(&self) -> Option<RequestLine<'a>> {
        let start = self.start.as_ref()?;
        Some(RequestLine {
            method: start.method_or_reason,
            target: start.path_or_status,
            version: start.version,
        })
    }

    /// Available on `Phase::ResponseHeaders`.
    pub fn response_line(&self) -> Option<ResponseLine<'a>> {
        let start = self.start.as_ref()?;
        if start.path_or_status.len() != 3 {
            return None;
        }
        let status = crate::parse_u16(start.path_or_status)?;
        if !(100..=599).contains(&status) {
            return None;
        }
        Some(ResponseLine {
            version: start.version,
            status,
            reason: start.method_or_reason,
        })
    }
}

struct StartLine<'a> {
    method_or_reason: &'a [u8],
    path_or_status: &'a [u8],
    version: &'a [u8],
}

pub struct RequestLine<'a> {
    pub method: &'a [u8],
    pub target: &'a [u8],
    pub version: &'a [u8],
}

pub struct ResponseLine<'a> {
    pub version: &'a [u8],
    pub status: u16,
    pub reason: &'a [u8],
}

#[derive(Clone, Copy)]
pub struct HeaderIndex<'a> {
    bytes: &'a [u8],
    blob: Blob<'a>,
}

fn entry_fields<'a>(entry: &[u8], blob: Blob<'a>) -> Option<(&'a [u8], &'a [u8])> {
    let name_len = u16_at(entry, 8)? as usize;
    if name_len == 0 {
        return None;
    }
    Some((
        blob.get(u32_at(entry, 0)?, name_len)?,
        blob.get(u32_at(entry, 4)?, u16_at(entry, 10)? as usize)?,
    ))
}

impl<'a> HeaderIndex<'a> {
    pub fn get(self, n: usize) -> Option<(&'a [u8], &'a [u8])> {
        entry_fields(
            view(self.bytes, n.checked_mul(ENTRY_LEN)?, ENTRY_LEN)?,
            self.blob,
        )
    }

    pub fn iter(self) -> impl Iterator<Item = (&'a [u8], &'a [u8])> {
        self.bytes
            .chunks_exact(ENTRY_LEN)
            .filter_map(move |entry| entry_fields(entry, self.blob))
    }

    pub fn first(self, name: &[u8]) -> Option<&'a [u8]> {
        self.iter()
            .find(|(key, _)| key.eq_ignore_ascii_case(name))
            .map(|(_, value)| value)
    }

    pub fn count(self, name: &[u8]) -> usize {
        self.iter()
            .filter(|(key, _)| key.eq_ignore_ascii_case(name))
            .count()
    }
}

/// The OUT window for the current invocation.
pub struct Output<'a> {
    buf: &'a mut [u8],
}

impl Output<'_> {
    /// A `None` state keeps the previous state; `Some(&[])` clears it.
    /// Check the result: a replacement or state that does not fit isn't sent.
    pub fn write(&mut self, mutation: Option<&[u8]>, state: Option<&[u8]>) -> bool {
        encode_output(self.buf, mutation, state).is_some()
    }
}

fn align(n: usize, boundary: usize) -> Option<usize> {
    Some(n.checked_add(boundary - 1)? & !(boundary - 1))
}

fn put16(dst: &mut [u8], off: usize, n: u16) {
    dst[off..off + 2].copy_from_slice(&n.to_le_bytes());
}

fn put32(dst: &mut [u8], off: usize, n: usize) {
    dst[off..off + 4].copy_from_slice(&(n as u32).to_le_bytes());
}

fn put_header(dst: &mut [u8], magic: u32, total: usize, count: usize, arena: usize, len: usize) {
    put32(dst, 0, magic as usize);
    put16(dst, 4, 1);
    put16(dst, 6, HDR_LEN as u16);
    put32(dst, 8, total);
    put16(dst, 12, count as u16);
    put16(dst, 14, SEC_STRIDE as u16);
    put32(dst, 16, arena);
    put32(dst, 20, len);
}

fn put_section(dst: &mut [u8], i: usize, kind: u16, offset: usize, len: usize, count: usize) {
    let base = HDR_LEN + i * SEC_STRIDE;
    put16(dst, base, kind);
    put32(dst, base + 4, offset);
    put32(dst, base + 8, len);
    put32(dst, base + 12, count);
}

/// Writes a complete OUT envelope without touching the destination on failure.
pub fn encode_output(
    dst: &mut [u8],
    mutation: Option<&[u8]>,
    state: Option<&[u8]>,
) -> Option<usize> {
    let effects = [(SEC_STATE, state), (SEC_MUTATION, mutation)];
    let count = effects.iter().filter(|(_, data)| data.is_some()).count();
    let arena = align(HDR_LEN + count * SEC_STRIDE, 8)?;
    let mut end = arena;
    for (_, bytes) in effects
        .iter()
        .filter_map(|(kind, data)| Some((*kind, (*data)?)))
    {
        end = align(end, 4)?.checked_add(bytes.len())?;
    }
    if end - arena > MAX_PAYLOAD {
        return None;
    }
    let total = align(end, 8)?;
    let dst = dst.get_mut(..total)?;
    dst.fill(0);
    put_header(dst, MAGIC_OUT, total, count, arena, end - arena);
    let mut off = arena;
    for (i, (kind, bytes)) in effects
        .into_iter()
        .filter_map(|(kind, data)| Some((kind, data?)))
        .enumerate()
    {
        off = align(off, 4)?;
        put_section(dst, i, kind, off, bytes.len(), 0);
        dst[off..off + bytes.len()].copy_from_slice(bytes);
        off += bytes.len();
    }
    Some(total)
}

#[cfg(target_arch = "wasm32")]
mod guest {
    use super::*;

    // The host's default IN capacity is 40 KiB and the OUT capacity is 4 KiB;
    // the exported IN window must cover both capacities in the ABI contract.
    pub const IN_LEN: usize = 44 * 1024;
    pub const OUT_LEN: usize = 4 * 1024;
    static mut IN_WINDOW: [u8; IN_LEN] = [0; IN_LEN];
    static mut OUT_WINDOW: [u8; OUT_LEN] = [0; OUT_LEN];

    pub fn buf_ptr() -> i32 {
        core::ptr::addr_of_mut!(IN_WINDOW) as i32
    }

    pub fn out_ptr() -> i32 {
        core::ptr::addr_of_mut!(OUT_WINDOW) as i32
    }

    /// # Safety
    /// The host owns window writes and serializes invocations of each guest.
    /// The handler may not reenter the ABI while these window borrows are live.
    pub unsafe fn invoke<F>(
        conn_id: i64,
        phase: i32,
        flags: i32,
        ptr: i32,
        len: i32,
        handler: F,
    ) -> i64
    where
        F: for<'a> FnOnce(Call, Event<'a>, &mut Output<'a>) -> i32,
    {
        if ptr != buf_ptr() || len < 0 || len as usize > IN_LEN || flags & !31 != 0 {
            return DENY as i64;
        }
        let Ok(phase) = Phase::try_from(phase) else {
            return DENY as i64;
        };
        let raw = unsafe {
            core::slice::from_raw_parts(core::ptr::addr_of!(IN_WINDOW).cast(), len as usize)
        };
        let Some(event) = Event::parse(raw) else {
            return DENY as i64;
        };
        let buf = unsafe {
            core::slice::from_raw_parts_mut(core::ptr::addr_of_mut!(OUT_WINDOW).cast(), OUT_LEN)
        };
        handler(
            Call {
                conn_id: conn_id as u64,
                phase,
                flags: Flags(flags as u32),
            },
            event,
            &mut Output { buf },
        ) as i64
    }
}

#[cfg(target_arch = "wasm32")]
#[doc(hidden)]
pub use guest::{buf_ptr, invoke, out_ptr, IN_LEN, OUT_LEN};

/// Exports metadata shared by lifecycle and event modules, and aborts on panic.
#[macro_export]
macro_rules! wg_v4_metadata {
    ($caps:expr) => {
        #[no_mangle]
        pub extern "C" fn wg_abi() -> i32 {
            $crate::v4::ABI
        }
        #[no_mangle]
        pub extern "C" fn wg_caps() -> i64 {
            $caps
        }
        #[panic_handler]
        fn panic(_: &core::panic::PanicInfo) -> ! {
            core::arch::wasm32::unreachable()
        }
    };
}

/// Exports a v4 event handler and fixed windows. The handler receives
/// `(Call, Event<'_>, &mut Output<'_>)` and returns a low-32-bit verdict.
#[macro_export]
macro_rules! wg_v4_plugin {
    ($caps:expr, $handler:path) => {
        $crate::wg_v4_metadata!($caps | $crate::v4::CAP_BLOB);
        #[no_mangle]
        pub extern "C" fn wg_buf_ptr() -> i32 {
            $crate::v4::buf_ptr()
        }
        #[no_mangle]
        pub extern "C" fn wg_buf_len() -> i32 {
            $crate::v4::IN_LEN as i32
        }
        #[no_mangle]
        pub extern "C" fn wg_out_ptr() -> i32 {
            $crate::v4::out_ptr()
        }
        #[no_mangle]
        pub extern "C" fn wg_out_len() -> i32 {
            $crate::v4::OUT_LEN as i32
        }
        /// # Safety
        /// The host must invoke at most one ABI call per Wasm instance at a time.
        #[no_mangle]
        pub unsafe extern "C" fn on_event(
            conn_id: i64,
            phase: i32,
            flags: i32,
            ptr: i32,
            len: i32,
        ) -> i64 {
            unsafe { $crate::v4::invoke(conn_id, phase, flags, ptr, len, $handler) }
        }
    };
}

#[cfg(test)]
mod tests {
    use super::*;

    fn input() -> [u8; 160] {
        // Independent IN fixture: absolute header offsets point into the blob
        // and the index and start-line structs are outside the arena.
        let blob = b"GET /hi HTTP/1.1\r\nHost: example\r\n\r\n";
        let mut b = [0; 160];
        let base = 72;
        put_header(&mut b, MAGIC_IN, 152, 3, base, blob.len());
        put_section(&mut b, 0, SEC_BLOB, base, blob.len(), 0);
        put_section(&mut b, 1, SEC_INDEX, 136, ENTRY_LEN, 1);
        put_section(&mut b, 2, SEC_START, 112, START_LEN, 0);
        b[base..base + blob.len()].copy_from_slice(blob);
        put32(&mut b, 112, base);
        put32(&mut b, 116, base + 4);
        put32(&mut b, 120, base + 8);
        put16(&mut b, 124, 3);
        put16(&mut b, 126, 3);
        put16(&mut b, 128, 8);
        put32(&mut b, 136, base + 18);
        put32(&mut b, 140, base + 24);
        put16(&mut b, 144, 4);
        put16(&mut b, 146, 7);
        b
    }

    #[test]
    fn reads_absolute_index_and_start_line() {
        let raw = input();
        let e = Event::parse(&raw).unwrap();
        assert_eq!(e.request_line().unwrap().target, b"/hi");
        assert_eq!(e.headers().first(b"host"), Some(&b"example"[..]));
        assert_eq!(e.state(), None);
    }

    #[test]
    fn rejects_bad_layouts() {
        for (offset, value) in [
            (4, 2u16),
            (14, 12),
            (HDR_LEN + 2, 1),
            (HDR_LEN + SEC_STRIDE, SEC_BLOB),
        ] {
            let mut raw = input();
            put16(&mut raw, offset, value);
            assert!(Event::parse(&raw).is_none());
        }
        for (offset, value) in [
            (20, 0xffff_ffff),
            (44, 137),
            (44, 112),
            (136, 18),
            (140, 152),
        ] {
            let mut raw = input();
            put32(&mut raw, offset, value);
            assert!(Event::parse(&raw).is_none());
        }
        assert!(Event::parse(&input()[..151]).is_none());
    }

    #[test]
    fn output_preserves_explicit_empty_state() {
        let mut raw = [0; 64];
        let n = encode_output(&mut raw, Some(b"ok"), Some(&[])).unwrap();
        let env = Envelope::parse(&raw[..n], MAGIC_OUT).unwrap();
        assert_eq!(env.count, 2);
        let state = env.sections[0];
        assert_eq!(state.kind, SEC_STATE);
        assert_eq!(env.section_bytes(state), b"");
        assert_eq!(env.section_bytes(env.sections[1]), b"ok");
        let n = encode_output(&mut raw, None, None).unwrap();
        assert_eq!(Envelope::parse(&raw[..n], MAGIC_OUT).unwrap().count, 0);
    }
}
