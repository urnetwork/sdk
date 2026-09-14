"""Exercise real strings, boolean ABI, callbacks, errors, and owned handles."""
import ctypes as C
import tempfile
import threading
import urnetwork as ur
from urnetwork._raw import urnet_get_by_jwt_cb

assert ur.raw.urnet_abi_version() == 1
assert len(ur._text(ur.raw.urnet_new_id())) == 36
start = ur.raw.urnet_live_handle_count()
assert "héllo" in ur._text(ur.raw.urnet_new_network_space_key("héllo".encode(), b"main"))
with ur.Handle(ur.raw.urnet_new_network_space_manager_no_storage()) as manager:
    ur.raw.urnet_network_space_manager_close(manager.handle)
assert ur.raw.urnet_live_handle_count() == start
with tempfile.TemporaryDirectory() as directory:
    handle = ur.raw.urnet_new_async_local_state(directory.encode())
    done = threading.Event()
    @urnet_get_by_jwt_cb
    def callback(user_data, result, ok):
        assert isinstance(ok, bool)
        done.set()
    ur.raw.urnet_async_local_state_get_by_jwt(handle, callback, None)
    assert done.wait(3), "native callback did not cross the ABI"
    ur.raw.urnet_async_local_state_close(handle)
    assert ur.raw.urnet_release(handle)
assert ur.raw.urnet_live_handle_count() == start
with ur.Device(2**63) as device:
    try:
        device.dial("tcp", "example.com:443")
        raise AssertionError("invalid native handle accepted")
    except ur.SocketError:
        pass
print("Python package:", ur.version())
