"""URnetwork devices and userspace sockets, backed by the CGo SDK."""
from __future__ import annotations
import ctypes as C
from dataclasses import dataclass
import json
import os
from pathlib import Path
import platform
import threading
import weakref
from ._raw import bind


def _load():
    system = {"Darwin": "darwin", "Linux": "linux", "Windows": "windows"}.get(platform.system())
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine())
    name = {"darwin": "libURnetworkSdk.dylib", "linux": "libURnetworkSdk.so", "windows": "URnetworkSdk.dll"}.get(system, "")
    path = os.environ.get("URNETWORK_SDK_LIBRARY") or str(Path(__file__).parent / "native" / f"{system}-{arch}" / name)
    lib = C.CDLL(path)  # CDLL releases the GIL during blocking socket operations.
    bind(lib)
    if lib.urnet_abi_version() != 1:
        raise ImportError("incompatible URnetwork native ABI")
    return lib


raw = _load()


def _text(pointer):
    if not pointer:
        return None
    try:
        return C.string_at(pointer).decode("utf-8")
    finally:
        raw.urnet_free_string(pointer)


def version() -> str:
    return _text(raw.urnet_version()) or ""


class SocketError(OSError):
    """A native error; partial bytes/data remain available to the caller."""
    def __init__(self, message, *, bytes_written=0, data=b""):
        super().__init__(message)
        self.bytes_written = bytes_written
        self.data = data


def _checked(error):
    message = _text(error.value)
    if message is not None:
        raise SocketError(message)


def _finish(handle, device=False):
    if device:
        raw.urnet_device_close(handle)
    raw.urnet_release(handle)  # Socket release also closes pending I/O.


class Handle:
    """Own a returned C ABI handle. Constructing transfers ownership."""
    def __init__(self, handle: int, *, device=False):
        if not handle:
            raise ValueError("a nonzero, owned native handle is required")
        self._handle = handle
        self._lock = threading.Lock()
        self._finalizer = weakref.finalize(self, _finish, handle, device)

    @property
    def handle(self):
        with self._lock:
            if not self._finalizer.alive:
                raise SocketError("closed handle")
            return self._handle

    def close(self):
        with self._lock:
            self._finalizer()

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


@dataclass(frozen=True)
class ReadResult:
    data: bytes
    eof: bool  # b"" with eof=False is an empty datagram.


class Conn(Handle):
    def read(self, capacity: int = 65535) -> ReadResult:
        if not 0 < capacity <= 16 * 1024 * 1024:
            raise ValueError("capacity must be 1..16777216")
        buffer = (C.c_uint8 * capacity)()
        eof, error = C.c_bool(), C.c_void_p()
        n = raw.urnet_conn_read(self.handle, buffer, capacity, C.byref(eof), C.byref(error))
        data = bytes(buffer[:max(0, n)])
        message = _text(error.value)
        if message is not None:
            raise SocketError(message, data=data)
        return ReadResult(data, eof.value)

    def write(self, data: bytes) -> int:
        data = bytes(data)
        if len(data) > 16 * 1024 * 1024:
            raise ValueError("write exceeds 16 MiB")
        buffer = (C.c_uint8 * len(data)).from_buffer_copy(data)
        error = C.c_void_p()
        n = raw.urnet_conn_write(self.handle, buffer, len(data), C.byref(error))
        message = _text(error.value)
        if message is not None:
            raise SocketError(message, bytes_written=max(n, 0))
        return n

    def _control(self, name, *args):
        error = C.c_void_p()
        getattr(raw, "urnet_conn_" + name)(self.handle, *args, C.byref(error))
        _checked(error)

    def set_deadline(self, epoch_millis: int = 0):
        self._control("set_deadline", epoch_millis)

    def set_read_deadline(self, epoch_millis: int = 0):
        self._control("set_read_deadline", epoch_millis)

    def set_write_deadline(self, epoch_millis: int = 0):
        self._control("set_write_deadline", epoch_millis)

    def close_read(self):
        self._control("close_read")

    def close_write(self):
        self._control("close_write")

    @property
    def local_address(self):
        return _text(raw.urnet_conn_local_addr(self.handle))

    @property
    def remote_address(self):
        return _text(raw.urnet_conn_remote_addr(self.handle))


class Device(Handle):
    def __init__(self, handle: int):
        super().__init__(handle, device=True)

    def dial(self, network: str, address: str, *, timeout_millis: int = 30000) -> Conn:
        return self._dial(network, address, timeout_millis, None)

    def dial_tls(self, network: str, address: str, *, timeout_millis: int = 30000,
                 server_name: str = "", root_ca_pem: str = "", next_protos=()) -> Conn:
        return self._dial(network, address, timeout_millis,
                          {"ServerName": server_name, "RootCAPEM": root_ca_pem, "NextProtos": list(next_protos)})

    def _dial(self, network, address, timeout, tls):
        if not 0 <= timeout <= 2147483647:
            raise ValueError("invalid timeout")
        error = C.c_void_p()
        args = [self.handle, network.encode(), address.encode(), timeout]
        fn = raw.urnet_device_dial
        if tls is not None:
            fn = raw.urnet_device_dial_tls
            args.append(json.dumps(tls).encode())
        handle = fn(*args, C.byref(error))
        _checked(error)
        return Conn(handle)


__all__ = ["Device", "Conn", "Handle", "ReadResult", "SocketError", "raw", "version"]
