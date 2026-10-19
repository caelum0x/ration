//! Safe Rust wrapper around `services/wal`'s C API (`wal.h`). This is the
//! ONLY module in this crate that uses `unsafe` — every other module
//! talks to the write-ahead log exclusively through [`Wal`] and
//! [`recover`].
//!
//! Mirrors `services/wal/wal.h` exactly:
//! - `wal_open` opens/creates the log file; returns `NULL` + a status
//!   code on failure instead of a `wal_status_t` return value.
//! - `wal_append` appends one length-prefixed, CRC32-checksummed frame
//!   and fsyncs before returning.
//! - `wal_read_next` reads sequentially into a caller-supplied buffer,
//!   returning `WAL_ERR_BUFFER_SMALL` (cursor untouched) if the buffer
//!   is too small — our wrapper hides this by growing its own buffer
//!   and retrying.
//! - `wal_recover` scans a WAL file by path, replaying every valid
//!   frame through a callback (in append order) and truncating any
//!   torn tail frame left by a crash, independent of any open handle.
//! - `wal_close` releases a handle without deleting the file.

use std::ffi::CString;
use std::os::raw::{c_char, c_int, c_void};
use std::path::Path;
use std::ptr;

use thiserror::Error;

/// Opaque handle matching C's `wal_t`. We never dereference this from
/// Rust — it only ever travels as a pointer across the FFI boundary.
#[repr(C)]
struct WalHandle {
    _opaque: [u8; 0],
}

#[allow(non_camel_case_types)]
type wal_t = WalHandle;

const WAL_OK: i32 = 0;
const WAL_ERR_IO: i32 = -1;
const WAL_ERR_INVALID: i32 = -2;
const WAL_ERR_CORRUPT: i32 = -3;
const WAL_ERR_EOF: i32 = -4;
const WAL_ERR_TOO_LARGE: i32 = -5;
const WAL_ERR_NOMEM: i32 = -6;
const WAL_ERR_BUFFER_SMALL: i32 = -7;

/// Mirrors C's `wal_recovery_stats_t`. `#[repr(C)]` with the exact same
/// field order/types so it can be passed by pointer to `wal_recover`.
#[repr(C)]
struct WalRecoveryStatsRaw {
    frames_recovered: usize,
    bytes_recovered: usize,
    torn_tail_dropped: c_int,
    torn_bytes_dropped: usize,
}

/// Matches C's `typedef int (*wal_recover_cb)(const uint8_t *payload, uint32_t len, void *ctx);`.
type WalRecoverCb = extern "C" fn(payload: *const u8, len: u32, ctx: *mut c_void) -> c_int;

extern "C" {
    fn wal_open(path: *const c_char, out_status: *mut i32) -> *mut wal_t;
    fn wal_append(w: *mut wal_t, payload: *const u8, len: u32) -> i32;
    fn wal_read_next(w: *mut wal_t, buf: *mut u8, buf_cap: u32, out_len: *mut u32) -> i32;
    fn wal_sync(w: *mut wal_t) -> i32;
    fn wal_close(w: *mut wal_t);
    fn wal_recover(
        path: *const c_char,
        cb: Option<WalRecoverCb>,
        ctx: *mut c_void,
        stats: *mut WalRecoveryStatsRaw,
    ) -> i32;
}

#[derive(Debug, Error, Clone, Copy, PartialEq, Eq)]
pub enum WalError {
    #[error("WAL I/O error")]
    Io,
    #[error("invalid argument passed to WAL, or recovery callback aborted")]
    Invalid,
    #[error("corrupt WAL frame (failed structural or CRC validation)")]
    Corrupt,
    #[error("unexpected end of WAL")]
    Eof,
    #[error("payload too large for a single WAL frame")]
    TooLarge,
    #[error("WAL ran out of memory")]
    Nomem,
    #[error("WAL read buffer too small")]
    BufferTooSmall,
    #[error("unknown WAL status code {0}")]
    Unknown(i32),
}

fn status_to_result(code: i32) -> Result<(), WalError> {
    match code {
        WAL_OK => Ok(()),
        WAL_ERR_IO => Err(WalError::Io),
        WAL_ERR_INVALID => Err(WalError::Invalid),
        WAL_ERR_CORRUPT => Err(WalError::Corrupt),
        WAL_ERR_EOF => Err(WalError::Eof),
        WAL_ERR_TOO_LARGE => Err(WalError::TooLarge),
        WAL_ERR_NOMEM => Err(WalError::Nomem),
        WAL_ERR_BUFFER_SMALL => Err(WalError::BufferTooSmall),
        other => Err(WalError::Unknown(other)),
    }
}

fn path_to_cstring(path: &Path) -> Result<CString, WalError> {
    let s = path.to_str().ok_or(WalError::Invalid)?;
    CString::new(s).map_err(|_| WalError::Invalid)
}

/// What a recovery scan found. Mirrors `wal_recovery_stats_t`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct RecoveryStats {
    pub frames_recovered: u64,
    pub bytes_recovered: u64,
    pub torn_tail_dropped: bool,
    pub torn_bytes_dropped: u64,
}

/// A safe handle to an open write-ahead log file.
///
/// Access is exclusive-by-construction: every method takes `&mut self`,
/// so the borrow checker guarantees Rust never issues two concurrent
/// FFI calls against the same handle. The underlying `libwal.a` does no
/// internal locking, so callers sharing a `Wal` across async tasks must
/// wrap it in a `Mutex` (see `crate::ledger::Ledger`).
pub struct Wal {
    ptr: *mut wal_t,
    /// Reused across `read_next` calls so repeated reads of
    /// similarly-sized frames don't reallocate every time.
    read_buf: Vec<u8>,
}

// Safety: `Wal` owns its `wal_t*` exclusively (only ever constructed by
// `Wal::open`, only ever destroyed by `Drop`), and every operation on
// the pointer requires `&mut self`, so moving a `Wal` to another thread
// and using it there is sound. It is NOT `Sync`: the C library does not
// synchronize concurrent access to the same handle.
unsafe impl Send for Wal {}

impl Wal {
    /// Opens (creating if necessary) the WAL file at `path`. Does not
    /// itself perform crash recovery — call [`recover`] on the same
    /// path first if the file may contain a torn tail frame from a
    /// crash, so the write cursor starts at a clean frame boundary.
    pub fn open(path: &Path) -> Result<Self, WalError> {
        let c_path = path_to_cstring(path)?;
        let mut status: i32 = WAL_OK;
        let ptr = unsafe { wal_open(c_path.as_ptr(), &mut status) };
        if ptr.is_null() {
            status_to_result(status)?;
            // wal_open returned NULL but status claimed WAL_OK — treat
            // defensively as an I/O error rather than fabricating a
            // handle from a null pointer.
            return Err(WalError::Io);
        }
        Ok(Wal {
            ptr,
            read_buf: Vec::new(),
        })
    }

    /// Appends `payload` as one new frame and fsyncs before returning.
    pub fn append(&mut self, payload: &[u8]) -> Result<(), WalError> {
        let len: u32 = payload.len().try_into().map_err(|_| WalError::TooLarge)?;
        let payload_ptr = if payload.is_empty() {
            ptr::null()
        } else {
            payload.as_ptr()
        };
        let rc = unsafe { wal_append(self.ptr, payload_ptr, len) };
        status_to_result(rc)
    }

    /// Forces an fsync of the underlying file descriptor. `append`
    /// already does this for every write; exposed for explicit
    /// durability assertions in tests.
    pub fn sync(&mut self) -> Result<(), WalError> {
        status_to_result(unsafe { wal_sync(self.ptr) })
    }

    /// Reads the next frame at the sequential read cursor. Returns
    /// `Ok(None)` at end-of-log, `Ok(Some(bytes))` on a valid frame, and
    /// `Err(WalError::Corrupt)` if the frame at the cursor is torn — the
    /// cursor is left there so repeated calls keep failing consistently
    /// until the file is fixed up by [`recover`]. Grows its internal
    /// buffer and retries transparently if a frame is larger than the
    /// current buffer (`WAL_ERR_BUFFER_SMALL`).
    pub fn read_next(&mut self) -> Result<Option<Vec<u8>>, WalError> {
        if self.read_buf.is_empty() {
            self.read_buf.resize(4096, 0);
        }
        loop {
            let mut out_len: u32 = 0;
            let cap: u32 = self
                .read_buf
                .len()
                .try_into()
                .unwrap_or(u32::MAX);
            let rc = unsafe {
                wal_read_next(self.ptr, self.read_buf.as_mut_ptr(), cap, &mut out_len)
            };
            match rc {
                WAL_OK => {
                    return Ok(Some(self.read_buf[..out_len as usize].to_vec()));
                }
                WAL_ERR_EOF => return Ok(None),
                WAL_ERR_BUFFER_SMALL => {
                    let new_cap = (self.read_buf.len() * 2).max(4096);
                    self.read_buf.resize(new_cap, 0);
                    continue;
                }
                other => return status_to_result(other).map(|_| None),
            }
        }
    }
}

impl Drop for Wal {
    fn drop(&mut self) {
        if !self.ptr.is_null() {
            unsafe { wal_close(self.ptr) };
            self.ptr = ptr::null_mut();
        }
    }
}

/// Trampoline bridging C's `wal_recover` callback convention to a Rust
/// closure. Monomorphized per closure type `F`, so `ctx` is simply that
/// closure's address cast to `*mut c_void` — no heap allocation, no
/// trait object, nothing double-checked at runtime beyond the cast
/// itself. Returns 0 (continue) if `F` returns `true`, 1 (abort) if it
/// returns `false`, matching `wal_recover_cb`'s documented contract.
extern "C" fn recover_trampoline<F: FnMut(&[u8]) -> bool>(
    payload: *const u8,
    len: u32,
    ctx: *mut c_void,
) -> c_int {
    // SAFETY: `ctx` was set by `recover` below to `&mut F as *mut c_void`
    // for the exact same `F`, and that reference is guaranteed to
    // outlive this call (it lives on `recover`'s stack for the duration
    // of the single `wal_recover` call that invokes this trampoline).
    let f: &mut F = unsafe { &mut *(ctx as *mut F) };
    // SAFETY: `wal_recover` guarantees `payload` is valid for `len`
    // bytes for the duration of this call only.
    let slice: &[u8] = if len == 0 {
        &[]
    } else {
        unsafe { std::slice::from_raw_parts(payload, len as usize) }
    };
    if f(slice) {
        0
    } else {
        1
    }
}

/// Scans the WAL file at `path`, replaying every valid frame (in append
/// order) through `callback`, and truncates any torn tail frame left by
/// a crash. `callback` returns `true` to continue, `false` to abort
/// recovery early (surfaced as `WalError::Invalid`).
///
/// Safe to call on a path that does not exist yet (the C implementation
/// creates it, scans zero frames, and reports `WAL_OK`).
pub fn recover<F: FnMut(&[u8]) -> bool>(
    path: &Path,
    mut callback: F,
) -> Result<RecoveryStats, WalError> {
    let c_path = path_to_cstring(path)?;
    let mut stats = WalRecoveryStatsRaw {
        frames_recovered: 0,
        bytes_recovered: 0,
        torn_tail_dropped: 0,
        torn_bytes_dropped: 0,
    };
    let ctx_ptr: *mut c_void = (&mut callback as *mut F).cast();
    let rc = unsafe {
        wal_recover(
            c_path.as_ptr(),
            Some(recover_trampoline::<F>),
            ctx_ptr,
            &mut stats,
        )
    };
    status_to_result(rc)?;
    Ok(RecoveryStats {
        frames_recovered: stats.frames_recovered as u64,
        bytes_recovered: stats.bytes_recovered as u64,
        torn_tail_dropped: stats.torn_tail_dropped != 0,
        torn_bytes_dropped: stats.torn_bytes_dropped as u64,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    #[test]
    fn append_and_read_back_roundtrip() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("test.wal");

        let mut w = Wal::open(&path).unwrap();
        w.append(b"hello").unwrap();
        w.append(b"world").unwrap();

        assert_eq!(w.read_next().unwrap(), Some(b"hello".to_vec()));
        assert_eq!(w.read_next().unwrap(), Some(b"world".to_vec()));
        assert_eq!(w.read_next().unwrap(), None);
    }

    #[test]
    fn read_next_handles_payloads_larger_than_the_initial_buffer() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("test.wal");
        let big = vec![0x42u8; 10_000]; // bigger than the 4096-byte initial buffer

        let mut w = Wal::open(&path).unwrap();
        w.append(&big).unwrap();
        assert_eq!(w.read_next().unwrap(), Some(big));
    }

    #[test]
    fn recover_on_missing_file_is_a_noop() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("missing.wal");
        let stats = recover(&path, |_| true).unwrap();
        assert_eq!(stats.frames_recovered, 0);
        assert!(!stats.torn_tail_dropped);
    }

    #[test]
    fn recover_replays_every_frame_in_order_via_callback() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("test.wal");

        {
            let mut w = Wal::open(&path).unwrap();
            w.append(b"one").unwrap();
            w.append(b"two").unwrap();
            w.append(b"three").unwrap();
        }

        let mut seen = Vec::new();
        let stats = recover(&path, |payload| {
            seen.push(payload.to_vec());
            true
        })
        .unwrap();

        assert_eq!(stats.frames_recovered, 3);
        assert_eq!(
            seen,
            vec![b"one".to_vec(), b"two".to_vec(), b"three".to_vec()]
        );
    }
}
