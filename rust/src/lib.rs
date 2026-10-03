//! URnetwork devices and sockets. The raw module exposes the complete C ABI.
//! Use recv() for UDP: an empty datagram is distinct from stream EOF.
pub mod raw;
mod runtime;
use std::{
    ffi::{CStr, CString}, io::{self, Read, Write}, path::PathBuf,
    sync::{atomic::{AtomicU64, Ordering}, OnceLock},
};
use sha2::{Digest, Sha256};

struct Runtime { raw: raw::Raw, _directory: Option<tempfile::TempDir> }
static RUNTIME: OnceLock<Result<Runtime, String>> = OnceLock::new();

pub fn native() -> io::Result<&'static raw::Raw> {
    match RUNTIME.get_or_init(|| {
        let mut directory = None;
        let path = if let Some(path) = std::env::var_os("URNETWORK_SDK_LIBRARY") {
            PathBuf::from(path)
        } else {
            let (compressed, hash, filename) = runtime::packaged().ok_or("No packaged runtime for this platform; run make -C rust native in a source checkout")?;
            let mut bytes = Vec::new();
            flate2::read::GzDecoder::new(compressed).read_to_end(&mut bytes).map_err(|e| e.to_string())?;
            if format!("{:x}", Sha256::digest(&bytes)) != hash { return Err("Native runtime checksum mismatch".into()); }
            let dir = tempfile::tempdir().map_err(|e| e.to_string())?;
            let path = dir.path().join(filename);
            std::fs::write(&path, &bytes).map_err(|e| e.to_string())?;
            directory = Some(dir);
            path
        };
        let raw = unsafe { raw::Raw::load(&path) }.map_err(|e| e.to_string())?;
        // Unix keeps the mapped library alive after unlink, avoiding one leaked
        // temporary directory per application process. Windows keeps it on disk.
        #[cfg(unix)]
        { directory.take(); }
        if unsafe { (raw.urnet_abi_version)() } != 1 { return Err("Incompatible URnetwork native ABI".into()); }
        // Go owns background threads: this process-wide runtime is never dlclosed.
        Ok(Runtime { raw, _directory: directory })
    }) {
        Ok(runtime) => Ok(&runtime.raw),
        Err(message) => Err(io::Error::other(message.clone())),
    }
}

/// # Safety
/// p must be an owned string returned by this runtime, or null.
pub unsafe fn take_string(p: *mut std::ffi::c_char) -> Option<String> {
    if p.is_null() { return None; }
    let s = unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned();
    unsafe { (native().expect("runtime loaded").urnet_free_string)(p) };
    Some(s)
}
fn string(s: &str) -> io::Result<CString> {
    CString::new(s).map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))
}
pub fn version() -> io::Result<String> {
    Ok(unsafe { take_string((native()?.urnet_version)()) }.unwrap_or_default())
}

pub struct Handle { value: AtomicU64, device: bool }
impl Handle {
    /// # Safety
    /// Transfer a valid owned handle from this runtime. Never wrap it twice.
    pub unsafe fn from_owned(value: u64) -> io::Result<Self> {
        native()?;
        if value == 0 { return Err(io::Error::new(io::ErrorKind::InvalidInput, "zero handle")); }
        Ok(Self { value: AtomicU64::new(value), device: false })
    }
    pub fn value(&self) -> io::Result<u64> {
        match self.value.load(Ordering::Acquire) {
            0 => Err(io::Error::new(io::ErrorKind::NotConnected, "closed handle")), v => Ok(v),
        }
    }
    pub fn close(&self) {
        let value = self.value.swap(0, Ordering::AcqRel);
        if value != 0 {
            let raw = native().expect("runtime loaded");
            unsafe { if self.device { (raw.urnet_device_close)(value); } (raw.urnet_release)(value); }
        }
    }
}
impl Drop for Handle { fn drop(&mut self) { self.close(); } }

pub struct Device(Handle);
impl Device {
    /// # Safety
    /// Transfer ownership of a valid native Device handle.
    pub unsafe fn from_owned(value: u64) -> io::Result<Self> {
        let mut h = unsafe { Handle::from_owned(value)? }; h.device = true; Ok(Self(h))
    }
    pub fn handle(&self) -> io::Result<u64> { self.0.value() }
    pub fn close(&self) { self.0.close(); }
    pub fn dial(&self, network: &str, address: &str, timeout_millis: i64) -> io::Result<Conn> {
        self.open(network, address, timeout_millis, None)
    }
    pub fn dial_tls(&self, network: &str, address: &str, timeout_millis: i64, tls_json: &str) -> io::Result<Conn> {
        self.open(network, address, timeout_millis, Some(tls_json))
    }
    fn open(&self, network: &str, address: &str, timeout: i64, tls: Option<&str>) -> io::Result<Conn> {
        let (raw, network, address) = (native()?, string(network)?, string(address)?);
        let mut error = std::ptr::null_mut();
        let h = unsafe {
            if let Some(tls) = tls { (raw.urnet_device_dial_tls)(self.handle()?, network.as_ptr(), address.as_ptr(), timeout, string(tls)?.as_ptr(), &mut error) }
            else { (raw.urnet_device_dial)(self.handle()?, network.as_ptr(), address.as_ptr(), timeout, &mut error) }
        };
        if let Some(e) = unsafe { take_string(error) } { return Err(io::Error::other(e)); }
        Ok(Conn { handle: unsafe { Handle::from_owned(h)? }, pending_read: None, pending_write: None })
    }
}

#[derive(Debug)]
pub struct ReadResult { pub data: Vec<u8>, pub eof: bool, pub error: Option<String> }
#[derive(Debug)]
pub struct WriteResult { pub count: usize, pub error: Option<String> }
pub struct Conn { handle: Handle, pending_read: Option<String>, pending_write: Option<String> }
impl Conn {
    pub fn close(&self) { self.handle.close(); }
    pub fn recv(&self, capacity: usize) -> io::Result<ReadResult> {
        if capacity == 0 || capacity > 16 * 1024 * 1024 { return Err(io::Error::new(io::ErrorKind::InvalidInput, "invalid capacity")); }
        let mut data = vec![0u8; capacity];
        let (mut eof, mut error) = (false, std::ptr::null_mut());
        let n = unsafe { (native()?.urnet_conn_read)(self.handle.value()?, data.as_mut_ptr(), capacity as i32, &mut eof, &mut error) };
        data.truncate(n.max(0) as usize);
        Ok(ReadResult { data, eof, error: unsafe { take_string(error) } })
    }
    pub fn send(&self, data: &[u8]) -> io::Result<WriteResult> {
        if data.len() > 16 * 1024 * 1024 { return Err(io::Error::new(io::ErrorKind::InvalidInput, "write exceeds 16 MiB")); }
        let mut error = std::ptr::null_mut();
        let n = unsafe { (native()?.urnet_conn_write)(self.handle.value()?, data.as_ptr(), data.len() as i32, &mut error) };
        Ok(WriteResult { count: n.max(0) as usize, error: unsafe { take_string(error) } })
    }
    fn control(&self, op: unsafe extern "C" fn(u64, i64, *mut *mut std::ffi::c_char) -> bool, millis: i64) -> io::Result<()> {
        let mut error = std::ptr::null_mut();
        unsafe { op(self.handle.value()?, millis, &mut error); }
        if let Some(e) = unsafe { take_string(error) } { return Err(io::Error::other(e)); } Ok(())
    }
    pub fn set_deadline(&self, epoch_millis: i64) -> io::Result<()> { self.control(native()?.urnet_conn_set_deadline, epoch_millis) }
    pub fn set_read_deadline(&self, epoch_millis: i64) -> io::Result<()> { self.control(native()?.urnet_conn_set_read_deadline, epoch_millis) }
    pub fn set_write_deadline(&self, epoch_millis: i64) -> io::Result<()> { self.control(native()?.urnet_conn_set_write_deadline, epoch_millis) }
    pub fn local_address(&self) -> io::Result<Option<String>> { Ok(unsafe { take_string((native()?.urnet_conn_local_addr)(self.handle.value()?)) }) }
    pub fn remote_address(&self) -> io::Result<Option<String>> { Ok(unsafe { take_string((native()?.urnet_conn_remote_addr)(self.handle.value()?)) }) }
}
impl Read for Conn {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        if out.is_empty() { return Ok(0); }
        if let Some(e) = self.pending_read.take() { return Err(io::Error::other(e)); }
        let result = self.recv(out.len())?;
        let n = result.data.len();
        out[..n].copy_from_slice(&result.data);
        if n == 0 { if let Some(e) = result.error { return Err(io::Error::other(e)); } }
        else { self.pending_read = result.error; }
        Ok(n)
    }
}
impl Write for Conn {
    fn write(&mut self, data: &[u8]) -> io::Result<usize> {
        if let Some(e) = self.pending_write.take() { return Err(io::Error::other(e)); }
        let result = self.send(data)?;
        if result.count == 0 { if let Some(e) = result.error { return Err(io::Error::other(e)); } }
        else { self.pending_write = result.error; }
        Ok(result.count)
    }
    fn flush(&mut self) -> io::Result<()> { Ok(()) }
}
