require "ffi"
require "json"
require_relative "urnetwork/raw"

module URnetwork
  module Raw
    extend FFI::Library
    system = case RUBY_PLATFORM
             when /darwin/ then "darwin"
             when /mingw|mswin/ then "windows"
             when /linux/ then "linux"
             else raise LoadError, "Unsupported URnetwork platform"
             end
    arch = case FFI::Platform::ARCH
           when "aarch64", "arm64" then "arm64"
           when "x86_64", "amd64" then "amd64"
           else raise LoadError, "Unsupported URnetwork architecture"
           end
    file = {"darwin" => "libURnetworkSdk.dylib", "linux" => "libURnetworkSdk.so", "windows" => "URnetworkSdk.dll"}[system]
    ffi_lib ENV["URNETWORK_SDK_LIBRARY"] || File.join(__dir__, "urnetwork", "native", "#{system}-#{arch}", file)
    bind_functions
    raise LoadError, "Incompatible URnetwork native ABI" unless urnet_abi_version == 1
  end

  def self.take_string(pointer)
    return nil if pointer.null?
    begin
      pointer.read_string.force_encoding(Encoding::UTF_8)
    ensure
      Raw.urnet_free_string(pointer)
    end
  end
  def self.version
    take_string(Raw.urnet_version)
  end

  class SocketError < IOError
    attr_reader :data, :bytes_written
    def initialize(message, data: "".b, bytes_written: 0)
      super(message)
      @data, @bytes_written = data, bytes_written
    end
  end

  # Takes ownership of a C ABI handle. Always close, or use Handle.open.
  class Handle
    def initialize(handle, device: false)
      raise ArgumentError, "Nonzero owned handle required" if handle == 0
      @handle, @device, @lock = handle, device, Mutex.new
    end
    def handle
      @lock.synchronize do
        raise SocketError, "Closed native handle" if @handle == 0
        @handle
      end
    end
    def close
      value = @lock.synchronize { h = @handle; @handle = 0; h }
      return if value == 0
      Raw.urnet_device_close(value) if @device
      Raw.urnet_release(value)
    end
    def self.open(handle)
      object = new(handle)
      return object unless block_given?
      begin
        yield object
      ensure
        object.close
      end
    end
  end

  class Device < Handle
    def initialize(handle)
      super(handle, device: true)
    end
    def dial(network, address, timeout_millis: 30_000)
      open_socket(network, address, timeout_millis, nil)
    end
    def dial_tls(network, address, timeout_millis: 30_000, **tls)
      open_socket(network, address, timeout_millis, JSON.generate(tls))
    end
    private
    def open_socket(network, address, timeout, tls)
      error = FFI::MemoryPointer.new(:pointer)
      h = if tls
            Raw.urnet_device_dial_tls(handle, network, address, timeout, tls, error)
          else
            Raw.urnet_device_dial(handle, network, address, timeout, error)
          end
      message = URnetwork.take_string(error.read_pointer)
      raise SocketError, message if message
      Conn.new(h)
    end
  end

  ReadResult = Struct.new(:data, :eof)
  class Conn < Handle
    def read(capacity = 65_535)
      raise ArgumentError, "Invalid read capacity" unless (1..16_777_216).cover?(capacity)
      data, eof, error = FFI::MemoryPointer.new(:uint8, capacity), FFI::MemoryPointer.new(:uint8), FFI::MemoryPointer.new(:pointer)
      n = Raw.urnet_conn_read(handle, data, capacity, eof, error)
      bytes = data.read_bytes([0, n].max)
      message = URnetwork.take_string(error.read_pointer)
      raise SocketError.new(message, data: bytes) if message
      ReadResult.new(bytes, eof.read_uint8 != 0)
    end
    def write(bytes)
      bytes = String(bytes).b
      raise ArgumentError, "Write exceeds 16 MiB" if bytes.bytesize > 16_777_216
      error = FFI::MemoryPointer.new(:pointer)
      n = Raw.urnet_conn_write(handle, FFI::MemoryPointer.from_string(bytes), bytes.bytesize, error)
      message = URnetwork.take_string(error.read_pointer)
      raise SocketError.new(message, bytes_written: [0, n].max) if message
      n
    end
    %w[set_deadline set_read_deadline set_write_deadline].each do |method|
      define_method(method) { |epoch_millis = 0| control(method, epoch_millis) }
    end
    %w[close_read close_write].each do |method|
      define_method(method) { control(method) }
    end
    def local_address
      URnetwork.take_string(Raw.urnet_conn_local_addr(handle))
    end
    def remote_address
      URnetwork.take_string(Raw.urnet_conn_remote_addr(handle))
    end
    private
    def control(method, *args)
      error = FFI::MemoryPointer.new(:pointer)
      Raw.public_send("urnet_conn_#{method}", handle, *args, error)
      message = URnetwork.take_string(error.read_pointer)
      raise SocketError, message if message
    end
  end
end
