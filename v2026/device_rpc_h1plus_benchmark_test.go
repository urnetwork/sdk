//go:build unix

package sdk

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/rpc"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/v2026"
)

// This is the real local native Device RPC path, not a bare Framer benchmark:
// pinned mutual TLS -> production local handler/dialer -> deviceRpcMux -> gob
// net/rpc -> verified echo. Only the OS socket connector is instrumented. The
// server uses an ephemeral httptest listener rather than rebinding a free port;
// its TLS config and HTTP upgrade handler are the production implementations.
// Every logical direction has exactly one in-flight request, with identical
// queue limits and no synthetic batching, extra buffering, or forced flushes.
// Duplex permits one simultaneous call on each of the existing mux streams.
//
// A measurement is per completed request+reply RPC, not per carrier frame. TLS
// socket writes are BELOW crypto/tls (record splitting included), not calls to
// tls.Conn.Write. Logical frames are counted independently at mux Write entry.
// Both endpoints, gob, payload/order verification and counters are included.
// Authentication, 16 warmup calls/direction, setup and teardown are excluded.
// Use build/bench-device-rpc.mjs for alternating fresh-process paired cohorts.

var deviceRpcBenchSizes = []int{256, 1200, 64 * 1024, int(deviceRpcDefaultMaxFrameBytes) - 4096}
var deviceRpcBenchDirections = []string{"forward", "reverse", "duplex"}

type DeviceRpcBenchmarkRequest struct {
	Sequence uint64
	Payload  []byte
}

type DeviceRpcBenchmarkReply struct {
	Sequence uint64
	Payload  []byte
}

type deviceRpcBenchEcho struct {
	mu       sync.Mutex
	next     uint64
	expected []byte
}

func (e *deviceRpcBenchEcho) Echo(request DeviceRpcBenchmarkRequest, reply *DeviceRpcBenchmarkReply) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if request.Sequence != e.next || !bytes.Equal(request.Payload, e.expected) {
		return fmt.Errorf("RPC request payload/order mismatch: sequence %d, expected %d", request.Sequence, e.next)
	}
	e.next++
	reply.Sequence, reply.Payload = request.Sequence, request.Payload
	return nil
}

type deviceRpcBenchCounts struct {
	writes atomic.Uint64
	bytes  atomic.Uint64
}

type deviceRpcBenchCountConn struct {
	net.Conn
	counts *deviceRpcBenchCounts
}

func (c *deviceRpcBenchCountConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.counts.writes.Add(1)
	c.counts.bytes.Add(uint64(n))
	return n, err
}

type deviceRpcBenchListener struct {
	net.Listener
	counts *deviceRpcBenchCounts
}

func (l *deviceRpcBenchListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &deviceRpcBenchCountConn{Conn: c, counts: l.counts}, nil
}

type deviceRpcBenchFixture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	listener  *WebsocketDeviceRpcListener
	server    *httptest.Server
	muxes     [2]*deviceRpcMux
	clients   [2]*rpc.Client
	services  [2]*deviceRpcBenchEcho
	served    chan struct{}
	tlsCounts deviceRpcBenchCounts
	frames    deviceRpcBenchCounts
	authed    atomic.Uint64
	log       *testingDeviceRpcAttemptLogger
	closed    bool
}

func newDeviceRpcBenchFixture(t testing.TB, framed bool, payload []byte) *deviceRpcBenchFixture {
	t.Helper()
	f := &deviceRpcBenchFixture{served: make(chan struct{}, 2), log: &testingDeviceRpcAttemptLogger{}}
	f.ctx, f.cancel = context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(func() { f.close(t) })
	keys, err := GenerateDeviceRpcKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.EnableH1Plus = framed
	settings.H1PlusStats = &connect.H1PlusStats{}
	settings.ClientSettings.Log = f.log
	settings.KeepAliveTimeout = 0 // no unrelated heartbeat enters a timed sample
	serverSettings := *settings
	serverSettings.H1PlusStats = &connect.H1PlusStats{}
	f.listener = NewWebsocketDeviceRpcListener(requireRemoteAddress("127.0.0.1:0"), keys.GetServerPem(), keys.GetClientCertPem(), &serverSettings)
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The production TLS pin verifier must run before this handler. A test
		// fixture must not accidentally benchmark plaintext or unauthenticated TLS.
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || len(r.TLS.PeerCertificates) != 1 {
			http.Error(w, "mutual TLS required", http.StatusUnauthorized)
			return
		}
		f.authed.Add(1)
		f.listener.handle(w, r)
	}))
	f.server.TLS, err = serverTlsConfig(settings.logger(), keys.GetServerPem(), keys.GetClientCertPem())
	if err != nil {
		t.Fatal(err)
	}
	f.server.TLS.NextProtos = []string{"http/1.1"}
	f.server.Listener = &deviceRpcBenchListener{Listener: f.server.Listener, counts: &f.tlsCounts}
	f.server.StartTLS()
	dialer := NewWebsocketDeviceRpcDialer(requireRemoteAddress(f.server.Listener.Addr().String()), keys.GetClientPem(), keys.GetServerCertPem(), settings)
	netDialer := &net.Dialer{Timeout: settings.RpcConnectTimeout, KeepAliveConfig: deviceRpcKeepAliveConfig(settings)}
	forward, _, err := dialer.dial(f.ctx, func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := netDialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &deviceRpcBenchCountConn{Conn: conn, counts: &f.tlsCounts}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.muxes[0] = forward.(*deviceRpcMuxConn).mux
	select {
	case f.muxes[1] = <-f.listener.accepts:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	for _, mux := range f.muxes {
		_, gotFramed := mux.ws.(*connect.FramedMessageConn)
		_, gotWebSocket := mux.ws.(*websocket.Conn)
		if gotFramed != framed || gotWebSocket == framed {
			t.Fatalf("requested framed=%t but negotiated %T", framed, mux.ws)
		}
		if mux.maxFrameBytes != deviceRpcDefaultMaxFrameBytes || mux.sendBytes.max != deviceRpcDefaultMaxQueuedBytes || mux.receiveBytes.max != deviceRpcDefaultMaxQueuedBytes {
			t.Fatal("benchmark changed production RPC frame/queue admission budgets")
		}
	}
	if f.authed.Load() != 1 || (framed && settings.H1PlusStats.Snapshot().Accepted != 1) {
		t.Fatal("benchmark did not establish exactly one authenticated selected carrier")
	}
	if payload == nil {
		return f // deterministic raw-mux receive-admission control below
	}
	for i := range 2 {
		// Forward: native remote calls local. Reverse: local calls native remote.
		caller, callee := f.muxes[i].conns[i], f.muxes[1-i].conns[i]
		f.services[i] = &deviceRpcBenchEcho{expected: payload}
		server := rpc.NewServer()
		if err := server.RegisterName("Benchmark", f.services[i]); err != nil {
			t.Fatal(err)
		}
		go func() {
			server.ServeConn(&deviceRpcBenchCountConn{Conn: callee, counts: &f.frames})
			f.served <- struct{}{}
		}()
		f.clients[i] = rpc.NewClient(&deviceRpcBenchCountConn{Conn: caller, counts: &f.frames})
	}
	return f
}

func (f *deviceRpcBenchFixture) close(t testing.TB) {
	t.Helper()
	if f.closed {
		return
	}
	f.closed = true
	f.cancel()
	for _, c := range f.clients {
		if c != nil {
			c.Close()
		}
	}
	for _, m := range f.muxes {
		if m != nil {
			m.close()
		}
	}
	if f.listener != nil {
		f.listener.Close()
	}
	if f.server != nil {
		f.server.Close()
	}
	for _, c := range f.clients {
		if c != nil {
			select {
			case <-f.served:
			case <-time.After(5 * time.Second):
				t.Error("RPC benchmark server did not join on close")
			}
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		drained := true
		for _, m := range f.muxes {
			if m == nil {
				continue
			}
			for _, budget := range []*deviceRpcByteBudget{m.sendBytes, m.receiveBytes} {
				budget.mu.Lock()
				drained = drained && budget.used == 0 && budget.waiters == 0
				budget.mu.Unlock()
			}
		}
		if drained {
			// testing.Cleanup retains f until the containing test returns. Clear
			// closed decoders/services here so post-close GC really measures the
			// released session, rather than this fixture's own references.
			f.clients = [2]*rpc.Client{}
			f.muxes = [2]*deviceRpcMux{}
			f.services = [2]*deviceRpcBenchEcho{}
			f.server, f.listener = nil, nil
			return
		}
		if time.Now().After(deadline) {
			t.Error("RPC benchmark leaked queued/in-flight bytes after close")
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func deviceRpcBenchCall(client *rpc.Client, payload []byte, sequence uint64) error {
	var reply DeviceRpcBenchmarkReply
	if err := client.Call("Benchmark.Echo", DeviceRpcBenchmarkRequest{Sequence: sequence, Payload: payload}, &reply); err != nil {
		return err
	}
	if reply.Sequence != sequence || !bytes.Equal(reply.Payload, payload) {
		return fmt.Errorf("RPC reply payload/order mismatch at sequence %d", sequence)
	}
	return nil
}

func deviceRpcBenchStreams(direction string) ([]int, error) {
	switch direction {
	case "forward":
		return []int{0}, nil
	case "reverse":
		return []int{1}, nil
	case "duplex":
		return []int{0, 1}, nil
	default:
		return nil, fmt.Errorf("unknown direction %q", direction)
	}
}

type deviceRpcBenchMemory struct {
	RuntimeBytes uint64 `json:"runtime_bytes"` // Sys minus HeapReleased
	HeapBytes    uint64 `json:"heap_bytes"`
	HeapObjects  uint64 `json:"heap_objects"`
}

func deviceRpcBenchMemoryAt(stats *runtime.MemStats) deviceRpcBenchMemory {
	return deviceRpcBenchMemory{RuntimeBytes: stats.Sys - stats.HeapReleased, HeapBytes: stats.HeapAlloc, HeapObjects: stats.HeapObjects}
}

type deviceRpcBenchMemorySampler struct {
	mu      sync.Mutex
	samples []metrics.Sample
	peak    deviceRpcBenchMemory
	count   int
}

func (s *deviceRpcBenchMemorySampler) sample() {
	s.mu.Lock()
	defer s.mu.Unlock()
	metrics.Read(s.samples)
	s.peak.RuntimeBytes = max(s.peak.RuntimeBytes, s.samples[0].Value.Uint64()-s.samples[1].Value.Uint64())
	s.peak.HeapBytes = max(s.peak.HeapBytes, s.samples[2].Value.Uint64())
	s.peak.HeapObjects = max(s.peak.HeapObjects, s.samples[3].Value.Uint64())
	s.count++
}

type deviceRpcBenchResult struct {
	Schema                    int                  `json:"schema"`
	Carrier                   string               `json:"carrier"`
	Direction                 string               `json:"direction"`
	PayloadBytes              int                  `json:"payload_bytes"`
	IterationsPerDirection    int                  `json:"iterations_per_direction"`
	CompletedRPCs             int                  `json:"completed_rpcs"`
	WarmupPerDirection        int                  `json:"warmup_per_direction"`
	MemoryDiagnostic          bool                 `json:"memory_diagnostic"`
	Correct                   bool                 `json:"correct"`
	ElapsedNS                 int64                `json:"elapsed_ns"`
	LatencyP50NS              int64                `json:"latency_p50_ns"`
	LatencyP95NS              int64                `json:"latency_p95_ns"`
	LatencyP99NS              int64                `json:"latency_p99_ns"`
	CPUNSPerRPC               float64              `json:"cpu_ns_per_rpc"`
	PayloadMbps               float64              `json:"payload_mbps"`
	AllocationsPerRPC         float64              `json:"allocations_per_rpc"`
	AllocatedBytesPerRPC      float64              `json:"allocated_bytes_per_rpc"`
	CarrierFramesPerRPC       float64              `json:"carrier_frames_per_rpc"`
	CarrierPayloadBytesPerRPC float64              `json:"carrier_payload_bytes_per_rpc"`
	TLSSocketWritesPerRPC     float64              `json:"tls_socket_writes_per_rpc"`
	TLSSocketBytesPerRPC      float64              `json:"tls_socket_bytes_per_rpc"`
	GCCount                   uint32               `json:"gc_count"`
	GCPauseNS                 uint64               `json:"gc_pause_ns"`
	Before                    deviceRpcBenchMemory `json:"before"`
	After                     deviceRpcBenchMemory `json:"after"`
	SampledPeak               deviceRpcBenchMemory `json:"sampled_peak"`
	SampleCount               int                  `json:"sample_count"`
	PostCloseGC               deviceRpcBenchMemory `json:"post_close_gc"`
	ProcessPeakRSSBytes       int64                `json:"process_peak_rss_bytes"`
	GoVersion                 string               `json:"go_version"`
	GOOS                      string               `json:"goos"`
	GOARCH                    string               `json:"goarch"`
	GOMAXPROCS                int                  `json:"gomaxprocs"`
}

func deviceRpcBenchUsage(t testing.TB) syscall.Rusage {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return usage
}

func deviceRpcBenchQuantile(sorted []int64, numerator, denominator int) int64 {
	// Nearest-rank percentiles; p50 is the reported per-call median.
	return sorted[max(0, (len(sorted)*numerator+denominator-1)/denominator-1)]
}

func measureDeviceRpc(t testing.TB, framed bool, direction string, size, iterations int, sampleMemory bool) deviceRpcBenchResult {
	t.Helper()
	streams, err := deviceRpcBenchStreams(direction)
	if err != nil || iterations < 1 || size < 1 || size > int(deviceRpcDefaultMaxFrameBytes)-4096 {
		t.Fatalf("invalid benchmark arguments: %s %d %d: %v", direction, size, iterations, err)
	}
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + i/251)
	}
	f := newDeviceRpcBenchFixture(t, framed, payload)
	const warmup = 16
	for _, stream := range streams {
		for i := range warmup {
			if err := deviceRpcBenchCall(f.clients[stream], payload, uint64(i)); err != nil {
				t.Fatal(err)
			}
		}
	}

	latency := make([]int64, iterations*len(streams))
	start := make(chan struct{})
	done := make(chan error, len(streams))
	var sampler *deviceRpcBenchMemorySampler
	if sampleMemory {
		sampler = &deviceRpcBenchMemorySampler{samples: []metrics.Sample{
			{Name: "/memory/classes/total:bytes"},
			{Name: "/memory/classes/heap/released:bytes"},
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/gc/heap/objects:objects"},
		}}
	}
	for j, stream := range streams {
		go func() {
			<-start
			for i := range iterations {
				begin := time.Now()
				if err := deviceRpcBenchCall(f.clients[stream], payload, uint64(warmup+i)); err != nil {
					done <- err
					return
				}
				latency[j*iterations+i] = time.Since(begin).Nanoseconds()
				if sampler != nil {
					sampler.sample()
				}
			}
			done <- nil
		}()
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	tlsWrites, tlsBytes := f.tlsCounts.writes.Load(), f.tlsCounts.bytes.Load()
	frameWrites, frameBytes := f.frames.writes.Load(), f.frames.bytes.Load()
	stopSample, samplerStopped := make(chan struct{}), make(chan struct{})
	if sampler != nil {
		sampler.sample()
		go func() {
			defer close(samplerStopped)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					sampler.sample()
				case <-stopSample:
					return
				}
			}
		}()
	}
	if b, ok := t.(*testing.B); ok {
		b.ResetTimer()
		b.StartTimer()
	}
	usageBefore := deviceRpcBenchUsage(t)
	begin := time.Now()
	close(start)
	for range streams {
		if err := <-done; err != nil {
			f.close(t)
			if sampler != nil {
				close(stopSample)
				<-samplerStopped
			}
			f.log.mu.Lock()
			lines := slices.Clone(f.log.lines)
			f.log.mu.Unlock()
			t.Fatalf("RPC workload failed: %v; mux diagnostics: %v", err, lines)
		}
	}
	elapsed := time.Since(begin)
	usageAfter := deviceRpcBenchUsage(t)
	if b, ok := t.(*testing.B); ok {
		b.StopTimer()
	}
	tlsWrites, tlsBytes = f.tlsCounts.writes.Load()-tlsWrites, f.tlsCounts.bytes.Load()-tlsBytes
	frameWrites, frameBytes = f.frames.writes.Load()-frameWrites, f.frames.bytes.Load()-frameBytes
	if sampler != nil {
		close(stopSample)
		<-samplerStopped
		sampler.sample()
	}
	runtime.ReadMemStats(&after)
	for _, stream := range streams {
		f.services[stream].mu.Lock()
		next := f.services[stream].next
		f.services[stream].mu.Unlock()
		if next != uint64(warmup+iterations) {
			t.Fatalf("stream %d verified %d requests, expected %d", stream, next, warmup+iterations)
		}
	}
	rpcs := iterations * len(streams)
	if frameWrites < uint64(2*rpcs) || tlsWrites < 1 || frameBytes < uint64(2*rpcs*size) {
		t.Fatalf("missing transport counters: frames=%d TLS writes=%d bytes=%d", frameWrites, tlsWrites, frameBytes)
	}
	slices.Sort(latency)
	result := deviceRpcBenchResult{
		Schema: 1, Carrier: "websocket", Direction: direction, PayloadBytes: size,
		IterationsPerDirection: iterations, CompletedRPCs: rpcs, WarmupPerDirection: warmup,
		MemoryDiagnostic: sampleMemory, Correct: true, ElapsedNS: elapsed.Nanoseconds(),
		LatencyP50NS: deviceRpcBenchQuantile(latency, 50, 100), LatencyP95NS: deviceRpcBenchQuantile(latency, 95, 100), LatencyP99NS: deviceRpcBenchQuantile(latency, 99, 100),
		CPUNSPerRPC:       float64(usageAfter.Utime.Nano()+usageAfter.Stime.Nano()-usageBefore.Utime.Nano()-usageBefore.Stime.Nano()) / float64(rpcs),
		PayloadMbps:       float64(size) * float64(rpcs) * 16 / elapsed.Seconds() / 1e6,
		AllocationsPerRPC: float64(after.Mallocs-before.Mallocs) / float64(rpcs), AllocatedBytesPerRPC: float64(after.TotalAlloc-before.TotalAlloc) / float64(rpcs),
		CarrierFramesPerRPC: float64(frameWrites) / float64(rpcs), CarrierPayloadBytesPerRPC: float64(frameBytes) / float64(rpcs),
		TLSSocketWritesPerRPC: float64(tlsWrites) / float64(rpcs), TLSSocketBytesPerRPC: float64(tlsBytes) / float64(rpcs),
		GCCount: after.NumGC - before.NumGC, GCPauseNS: after.PauseTotalNs - before.PauseTotalNs,
		Before: deviceRpcBenchMemoryAt(&before), After: deviceRpcBenchMemoryAt(&after),
		ProcessPeakRSSBytes: usageAfter.Maxrss,
		GoVersion:           runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GOMAXPROCS: runtime.GOMAXPROCS(0),
	}
	if runtime.GOOS != "darwin" {
		result.ProcessPeakRSSBytes *= 1024 // Getrusage uses bytes on Darwin, KiB on other measured Unix hosts.
	}
	if framed {
		result.Carrier = "framerxl"
	}
	if sampler != nil {
		result.SampledPeak, result.SampleCount = sampler.peak, sampler.count
	}
	f.close(t)
	// Release gob client/decoder buffers and fixture pointers before post-close
	// GC; the result deliberately retains no connection or full payload.
	f = nil
	payload = nil
	runtime.GC()
	var postClose runtime.MemStats
	runtime.ReadMemStats(&postClose)
	result.PostCloseGC = deviceRpcBenchMemoryAt(&postClose)
	return result
}

// Durable runner entry point: exactly one observation per fresh test process.
// It is skipped in ordinary suites; correctness gates below always run.
func TestDeviceRpcH1PlusMeasurement(t *testing.T) {
	mode := os.Getenv("DEVICE_RPC_BENCH_CARRIER")
	if mode == "" {
		t.Skip("use build/bench-device-rpc.mjs for explicit fresh-process measurements")
	}
	if mode != "websocket" && mode != "framerxl" {
		t.Fatalf("invalid measurement carrier %q", mode)
	}
	size, err := strconv.Atoi(os.Getenv("DEVICE_RPC_BENCH_PAYLOAD"))
	if err != nil {
		t.Fatal(err)
	}
	iterations, err := strconv.Atoi(os.Getenv("DEVICE_RPC_BENCH_ITERATIONS"))
	if err != nil {
		t.Fatal(err)
	}
	result := measureDeviceRpc(t, mode == "framerxl", os.Getenv("DEVICE_RPC_BENCH_DIRECTION"), size, iterations, os.Getenv("DEVICE_RPC_BENCH_MEMORY") == "1")
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("DEVICE_RPC_MEASUREMENT %s\n", encoded)
}

func BenchmarkDeviceRpcH1PlusMTLS(b *testing.B) {
	for _, size := range deviceRpcBenchSizes {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			for _, direction := range deviceRpcBenchDirections {
				b.Run(direction, func(b *testing.B) {
					if !deviceRpcBenchAdmissionSupported(size, direction) {
						b.Skip("two near-limit payloads exceed the default shared receive budget; see saturation control")
					}
					for _, framed := range []bool{false, true} {
						name := "websocket"
						if framed {
							name = "framerxl"
						}
						b.Run(name, func(b *testing.B) {
							b.StopTimer()
							r := measureDeviceRpc(b, framed, direction, size, b.N, false)
							b.ReportMetric(float64(r.ElapsedNS)/float64(r.CompletedRPCs), "wall-ns/rpc")
							b.ReportMetric(float64(r.LatencyP50NS), "p50-ns/rpc")
							b.ReportMetric(float64(r.LatencyP95NS), "p95-ns/rpc")
							b.ReportMetric(r.CPUNSPerRPC, "cpu-ns/rpc")
							b.ReportMetric(r.PayloadMbps, "payload-Mb/s")
							b.ReportMetric(r.AllocationsPerRPC, "allocs/rpc")
							b.ReportMetric(r.AllocatedBytesPerRPC, "B/rpc")
							b.ReportMetric(r.TLSSocketWritesPerRPC, "tls-socket-writes/rpc")
						})
					}
				})
			}
		})
	}
}

func TestDeviceRpcH1PlusBenchmarkCorrectness(t *testing.T) {
	for _, size := range deviceRpcBenchSizes {
		for _, framed := range []bool{false, true} {
			for _, direction := range deviceRpcBenchDirections {
				if !deviceRpcBenchAdmissionSupported(size, direction) {
					continue // this is an asserted negative case, not a speed result
				}
				t.Run(fmt.Sprintf("%d/%s/framed=%t", size, direction, framed), func(t *testing.T) {
					r := measureDeviceRpc(t, framed, direction, size, 3, false)
					if !r.Correct || r.LatencyP50NS <= 0 || r.LatencyP95NS < r.LatencyP50NS || r.CarrierFramesPerRPC < 2 || r.TLSSocketWritesPerRPC <= 0 {
						t.Fatalf("invalid correct measurement: %+v", r)
					}
				})
			}
		}
	}
}

func deviceRpcBenchAdmissionSupported(size int, direction string) bool {
	// One request+reply stream works at the existing 3 MiB message cap. Duplex
	// can deliver an incoming request on one tag and reply on the other before
	// either consumer releases its frame. Leave gob/tag headroom in this test
	// policy; do not grow the real shared 4 MiB receive budget to hide saturation.
	return direction != "duplex" || int64(size+4096)*2 <= deviceRpcDefaultMaxQueuedBytes
}

func TestDeviceRpcH1PlusBenchmarkLargeDuplexAdmission(t *testing.T) {
	for _, framed := range []bool{false, true} {
		t.Run(fmt.Sprintf("framed=%t", framed), func(t *testing.T) {
			f := newDeviceRpcBenchFixture(t, framed, nil)
			payload := make([]byte, deviceRpcDefaultMaxFrameBytes-4096)
			if int64(len(payload)+1)*2 <= deviceRpcDefaultMaxQueuedBytes {
				t.Fatal("saturation control no longer exceeds the real receive budget")
			}
			// No receiver consumes the first stream. Both independently valid
			// frames traverse real pinned mTLS; the second cannot be admitted.
			for _, stream := range []int{0, 1} {
				if n, err := f.muxes[0].conns[stream].Write(payload); err != nil || n != len(payload) {
					t.Fatalf("send stream %d: %d %v", stream, n, err)
				}
			}
			select {
			case <-f.muxes[1].ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("saturated receive budget did not close the real RPC carrier")
			}
			f.close(t) // also asserts joined servers and zero queued/in-flight bytes
			f.log.mu.Lock()
			defer f.log.mu.Unlock()
			if !slices.Contains(f.log.lines, "[mux]receive byte budget full; closing rpc generation") {
				t.Fatalf("large duplex failed for a different reason: %v", f.log.lines)
			}
		})
	}
}

func TestDeviceRpcH1PlusBenchmarkControls(t *testing.T) {
	// Prove verification fails on corrupt/out-of-order traffic, not just that a
	// byte count was received. Production carrier selection, TLS pins and budget
	// drainage are also asserted by every measurement fixture.
	e := &deviceRpcBenchEcho{expected: []byte{1, 2, 3}}
	var reply DeviceRpcBenchmarkReply
	for _, request := range []DeviceRpcBenchmarkRequest{
		{Sequence: 1, Payload: []byte{1, 2, 3}},
		{Sequence: 0, Payload: []byte{1, 2, 4}},
		{Sequence: 0, Payload: []byte{1, 2}},
	} {
		if err := e.Echo(request, &reply); err == nil {
			t.Fatal("benchmark accepted corrupt/order-invalid payload")
		}
	}
	if err := e.Echo(DeviceRpcBenchmarkRequest{Payload: []byte{1, 2, 3}}, &reply); err != nil {
		t.Fatal(err)
	}
	if err := e.Echo(DeviceRpcBenchmarkRequest{Payload: []byte{1, 2, 3}}, &reply); err == nil {
		t.Fatal("benchmark accepted duplicate sequence")
	}
	if _, err := deviceRpcBenchStreams("unknown"); err == nil {
		t.Fatal("benchmark accepted unknown direction")
	}
	r := measureDeviceRpc(t, true, "duplex", 1200, 8, true)
	if r.SampleCount < r.CompletedRPCs || r.SampledPeak.RuntimeBytes == 0 || r.SampledPeak.HeapBytes == 0 || r.PostCloseGC.RuntimeBytes == 0 {
		t.Fatalf("memory diagnostic was not sampled: %+v", r)
	}
}
