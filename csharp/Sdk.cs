using System.Reflection;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;

namespace URnetwork.SDK;

public static partial class Raw
{
    static Raw()
    {
        NativeLibrary.SetDllImportResolver(typeof(Raw).Assembly, (name, assembly, search) => {
            if (name != "URnetworkSdk") return IntPtr.Zero;
            string os = OperatingSystem.IsWindows() ? "win" : OperatingSystem.IsMacOS() ? "osx" : "linux";
            string arch = RuntimeInformation.ProcessArchitecture switch {
                Architecture.Arm64 => "arm64", Architecture.X64 => "x64",
                _ => throw new PlatformNotSupportedException("URnetwork requires a 64-bit supported runtime")
            };
            string file = os == "win" ? "URnetworkSdk.dll" : os == "osx" ? "libURnetworkSdk.dylib" : "libURnetworkSdk.so";
            string? path = Environment.GetEnvironmentVariable("URNETWORK_SDK_LIBRARY");
            string baseDir = Path.GetDirectoryName(assembly.Location) ?? AppContext.BaseDirectory;
            path ??= Path.Combine(baseDir, "runtimes", $"{os}-{arch}", "native", file);
            // dotnet publish may flatten the selected RID's native assets.
            if (!File.Exists(path) && File.Exists(Path.Combine(baseDir, file))) path = Path.Combine(baseDir, file);
            return NativeLibrary.Load(path);
        });
        if (urnet_abi_version() != 1) throw new TypeLoadException("Incompatible URnetwork native ABI");
    }
}

public static class Sdk
{
    public static string Version => TakeString(Raw.urnet_version()) ?? "";
    public static string? TakeString(IntPtr owned)
    {
        if (owned == IntPtr.Zero) return null;
        try { return Marshal.PtrToStringUTF8(owned); }
        finally { Raw.urnet_free_string(owned); }
    }
    internal static void Checked(IntPtr error)
    {
        string? message = TakeString(error);
        if (message != null) throw new IOException(message);
    }
}

/// Owns a C ABI handle. Constructing transfers ownership; do not release it twice.
public class Handle : SafeHandleZeroOrMinusOneIsInvalid
{
    private readonly bool device;
    public Handle(ulong ownedHandle, bool device = false) : base(true)
    {
        if (ownedHandle == 0) throw new ArgumentException("Nonzero owned handle required");
        this.device = device;
        SetHandle(unchecked((IntPtr)(long)ownedHandle));
    }
    public ulong Value => !IsClosed ? unchecked((ulong)handle.ToInt64()) : throw new ObjectDisposedException(nameof(Handle));
    protected override bool ReleaseHandle()
    {
        ulong value = unchecked((ulong)handle.ToInt64());
        if (device) Raw.urnet_device_close(value);
        return Raw.urnet_release(value) != 0;
    }
}

public sealed class Device : Handle
{
    public Device(ulong ownedHandle) : base(ownedHandle, true) {}
    public Conn Dial(string network, string address, long timeoutMillis = 30000)
    {
        ulong h = Raw.urnet_device_dial(Value, network, address, timeoutMillis, out var error);
        Sdk.Checked(error); return new Conn(h);
    }
    public Conn DialTls(string network, string address, string tlsJson = "{}", long timeoutMillis = 30000)
    {
        ulong h = Raw.urnet_device_dial_tls(Value, network, address, timeoutMillis, tlsJson, out var error);
        Sdk.Checked(error); return new Conn(h);
    }
}

public sealed class SocketException : IOException
{
    public byte[] PartialData { get; }
    public int BytesWritten { get; }
    internal SocketException(string message, byte[] data, int written) : base(message)
    { PartialData = data; BytesWritten = written; }
}

public readonly record struct ReadResult(byte[] Data, bool Eof);

/// Blocking socket operations. Close/Dispose interrupts pending I/O.
public sealed class Conn : Handle
{
    public Conn(ulong ownedHandle) : base(ownedHandle) {}
    public ReadResult Read(int capacity = 65535)
    {
        if (capacity <= 0 || capacity > 16 * 1024 * 1024) throw new ArgumentOutOfRangeException(nameof(capacity));
        IntPtr buffer = Marshal.AllocHGlobal(capacity);
        try {
            int n = Raw.urnet_conn_read(Value, buffer, capacity, out byte eof, out var error);
            byte[] data = new byte[Math.Max(n, 0)];
            Marshal.Copy(buffer, data, 0, data.Length);
            string? message = Sdk.TakeString(error);
            if (message != null) throw new SocketException(message, data, 0);
            return new ReadResult(data, eof != 0);
        } finally { Marshal.FreeHGlobal(buffer); }
    }
    public int Write(byte[] data)
    {
        if (data.Length > 16 * 1024 * 1024) throw new ArgumentOutOfRangeException(nameof(data));
        IntPtr buffer = Marshal.AllocHGlobal(Math.Max(1, data.Length));
        try {
            Marshal.Copy(data, 0, buffer, data.Length);
            int n = Raw.urnet_conn_write(Value, buffer, data.Length, out var error);
            string? message = Sdk.TakeString(error);
            if (message != null) throw new SocketException(message, Array.Empty<byte>(), Math.Max(n, 0));
            return n;
        } finally { Marshal.FreeHGlobal(buffer); }
    }
    public void SetDeadline(long epochMillis = 0) { Raw.urnet_conn_set_deadline(Value, epochMillis, out var e); Sdk.Checked(e); }
    public void SetReadDeadline(long epochMillis = 0) { Raw.urnet_conn_set_read_deadline(Value, epochMillis, out var e); Sdk.Checked(e); }
    public void SetWriteDeadline(long epochMillis = 0) { Raw.urnet_conn_set_write_deadline(Value, epochMillis, out var e); Sdk.Checked(e); }
    public void CloseRead() { Raw.urnet_conn_close_read(Value, out var e); Sdk.Checked(e); }
    public void CloseWrite() { Raw.urnet_conn_close_write(Value, out var e); Sdk.Checked(e); }
    public string? LocalAddress => Sdk.TakeString(Raw.urnet_conn_local_addr(Value));
    public string? RemoteAddress => Sdk.TakeString(Raw.urnet_conn_remote_addr(Value));
}
