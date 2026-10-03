package io.ur.sdk;

import com.sun.jna.*;
import com.sun.jna.ptr.*;
import java.io.IOException;
import java.lang.ref.Cleaner;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;

/** Desktop JVM binding. Android applications should use the gomobile AAR. */
public final class Sdk {
    private Sdk() {}
    public static final Raw raw = load();
    private static final Cleaner CLEANER = Cleaner.create();

    private static Raw load() {
        String os = System.getProperty("os.name").toLowerCase();
        String system = os.contains("mac") ? "darwin" : os.contains("win") ? "windows" : os.contains("linux") ? "linux" : "";
        String cpu = System.getProperty("os.arch").toLowerCase();
        String arch = cpu.equals("aarch64") || cpu.equals("arm64") ? "arm64" : cpu.equals("amd64") || cpu.equals("x86_64") ? "amd64" : "";
        String name = system.equals("windows") ? "URnetworkSdk.dll" : system.equals("darwin") ? "libURnetworkSdk.dylib" : "libURnetworkSdk.so";
        String path = System.getenv("URNETWORK_SDK_LIBRARY");
        try {
            if (path == null) path = Native.extractFromResourcePath("/native/" + system + "-" + arch + "/" + name, Sdk.class.getClassLoader()).getAbsolutePath();
        } catch (IOException e) {
            throw new IllegalStateException("No URnetwork runtime for " + system + "-" + arch, e);
        }
        Raw result = Native.load(path, Raw.class, Map.of(Library.OPTION_STRING_ENCODING, "UTF-8"));
        if (result.urnet_abi_version() != 1) throw new IllegalStateException("Incompatible URnetwork native ABI");
        return result;
    }

    /** Copy and free an owned C ABI string. */
    public static String takeString(Pointer p) {
        if (p == null) return null;
        try { return p.getString(0, "UTF-8"); }
        finally { raw.urnet_free_string(p); }
    }
    public static String version() { return takeString(raw.urnet_version()); }
    private static void checked(PointerByReference error) throws IOException {
        String message = takeString(error.getValue());
        if (message != null) throw new IOException(message);
    }

    private static final class Owner implements Runnable {
        final AtomicLong handle;
        final boolean device;
        Owner(long handle, boolean device) { this.handle = new AtomicLong(handle); this.device = device; }
        public void run() {
            long h = handle.getAndSet(0);
            if (h == 0) return;
            if (device) raw.urnet_device_close(h);
            raw.urnet_release(h);
        }
    }

    /** Takes ownership of a native handle. Use try-with-resources. */
    public static class Handle implements AutoCloseable {
        private final Owner owner;
        private final Cleaner.Cleanable cleanable;
        public Handle(long handle) { this(handle, false); }
        protected Handle(long handle, boolean device) {
            if (handle == 0) throw new IllegalArgumentException("Nonzero owned handle required");
            owner = new Owner(handle, device);
            cleanable = CLEANER.register(this, owner);
        }
        public final long handle() {
            long h = owner.handle.get();
            if (h == 0) throw new IllegalStateException("Closed native handle");
            return h;
        }
        @Override public void close() { cleanable.clean(); }
    }

    public static final class Device extends Handle {
        public Device(long ownedHandle) { super(ownedHandle, true); }
        public Conn dial(String network, String address) throws IOException { return dial(network, address, 30000); }
        public Conn dial(String network, String address, long timeoutMillis) throws IOException {
            PointerByReference error = new PointerByReference();
            long h = raw.urnet_device_dial(handle(), network, address, timeoutMillis, error);
            checked(error);
            return new Conn(h);
        }
        /** tlsJson contains ServerName, RootCAPEM and NextProtos; {} uses verified defaults. */
        public Conn dialTls(String network, String address, long timeoutMillis, String tlsJson) throws IOException {
            PointerByReference error = new PointerByReference();
            long h = raw.urnet_device_dial_tls(handle(), network, address, timeoutMillis, tlsJson, error);
            checked(error);
            return new Conn(h);
        }
    }

    public record ReadResult(byte[] data, boolean eof) {}
    public static final class SocketException extends IOException {
        public final byte[] data;
        public final int bytesWritten;
        SocketException(String message, byte[] data, int bytesWritten) {
            super(message); this.data = data; this.bytesWritten = bytesWritten;
        }
    }

    public static final class Conn extends Handle {
        public Conn(long ownedHandle) { super(ownedHandle); }
        /** EOF is separate from an empty UDP datagram. Blocks the calling thread. */
        public ReadResult read(int capacity) throws IOException {
            if (capacity <= 0 || capacity > 16 * 1024 * 1024) throw new IllegalArgumentException("Invalid read capacity");
            try (Memory bytes = new Memory(capacity)) {
                ByteByReference eof = new ByteByReference();
                PointerByReference error = new PointerByReference();
                int n = raw.urnet_conn_read(handle(), bytes, capacity, eof, error);
                byte[] data = bytes.getByteArray(0, Math.max(0, n));
                String message = takeString(error.getValue());
                if (message != null) throw new SocketException(message, data, 0);
                return new ReadResult(data, eof.getValue() != 0);
            }
        }
        public int write(byte[] data) throws IOException {
            if (data.length > 16 * 1024 * 1024) throw new IllegalArgumentException("Write exceeds 16 MiB");
            try (Memory bytes = new Memory(Math.max(1, data.length))) {
                bytes.write(0, data, 0, data.length);
                PointerByReference error = new PointerByReference();
                int n = raw.urnet_conn_write(handle(), bytes, data.length, error);
                String message = takeString(error.getValue());
                if (message != null) throw new SocketException(message, new byte[0], Math.max(0, n));
                return n;
            }
        }
        public void setDeadline(long epochMillis) throws IOException {
            PointerByReference e = new PointerByReference(); raw.urnet_conn_set_deadline(handle(), epochMillis, e); checked(e);
        }
        public void setReadDeadline(long epochMillis) throws IOException {
            PointerByReference e = new PointerByReference(); raw.urnet_conn_set_read_deadline(handle(), epochMillis, e); checked(e);
        }
        public void setWriteDeadline(long epochMillis) throws IOException {
            PointerByReference e = new PointerByReference(); raw.urnet_conn_set_write_deadline(handle(), epochMillis, e); checked(e);
        }
        public void closeRead() throws IOException {
            PointerByReference e = new PointerByReference(); raw.urnet_conn_close_read(handle(), e); checked(e);
        }
        public void closeWrite() throws IOException {
            PointerByReference e = new PointerByReference(); raw.urnet_conn_close_write(handle(), e); checked(e);
        }
        public String localAddress() { return takeString(raw.urnet_conn_local_addr(handle())); }
        public String remoteAddress() { return takeString(raw.urnet_conn_remote_addr(handle())); }
    }
}
