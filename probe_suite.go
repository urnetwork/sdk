package sdk

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// A test suite that runs real traffic through the live tunnel and times it.
//
// The problem this solves is that every reliability change so far has been
// judged by how long a freeze felt in a browser. That confounds two things:
// what the tunnel did, and what the browser decided to do about it. A browser
// sits on its own backoff after a reset, so it reports a number that no
// client-side fix can move -- which makes a working fix and a useless one look
// identical.
//
// These probes measure only the first half. A probe reconnects the instant it
// is told to, so the interval it reports is the tunnel's contribution alone.
// That number will read better than lived experience; the two are different
// quantities and are not interchangeable.
//
// # Why not an ordinary http client
//
// The app excludes itself from its own tunnel (`addDisallowedApplication` on
// Android, and the equivalent elsewhere), because that exclusion is what stops
// the platform transport's own socket from routing back through the tunnel it
// is carrying. So an http client on an OS socket measures the bare connection
// and reports excellent numbers that say nothing at all.
//
// Instead a `connect.Tun` -- a userspace gvisor stack, packets on one side and
// sockets on the other -- is pumped into the live device. Probe traffic takes
// the same exits, contracts, and affinity as the browser's, entirely below the
// OS socket layer, so there is no exclusion to defeat and no loop to create.
// StallExit hits a probe exactly as it hits a real flow.

const (
	// probeUserAgent identifies probe traffic at the destination. Probes hit
	// third-party sites, and anyone reading their logs should be able to tell
	// what this is rather than seeing unexplained requests.
	probeUserAgent = "urnetwork-probe/1"
	// probeIdleTimeout bounds the pump goroutines after a run ends.
	probeShutdownTimeout = 2 * time.Second
)

// ProbeResult is one timed request. Durations are milliseconds (gomobile does
// not bind time.Duration) and -1 marks a phase that did not happen -- a reused
// connection has no dns or connect phase, and conflating "did not occur" with
// "took no time" would quietly halve an average.
type ProbeResult struct {
	// Name is the target, e.g. "example.com" or "cdn 1MiB".
	Name string
	// Kind is "dns", "http", or "download".
	Kind string

	Ok bool
	// Error is empty when Ok. Kept as a string because the failure mode is
	// itself a result -- "connection reset" and "timeout" mean opposite things
	// about whether teardown reached the client.
	Error string

	DnsMillis     int64
	ConnectMillis int64
	TtfbMillis    int64
	TotalMillis   int64
	ByteCount     int64

	// StartOffsetMillis is when this probe started, relative to the run. It is
	// what lines a failure up against the moment an exit was stalled.
	StartOffsetMillis int64
}

type ProbeResultList struct {
	exportedList[*ProbeResult]
}

func NewProbeResultList() *ProbeResultList {
	return &ProbeResultList{
		exportedList: *newExportedList[*ProbeResult](),
	}
}

// ProbeSuiteConfig is fixed rather than sampled from the environment, because
// an A/B comparison is only valid if both runs did identical work.
type ProbeSuiteConfig struct {
	// Concurrency is how many probes run at once. 1 measures latency; higher
	// values are what expose head-of-line blocking, since flows sharing an
	// exit share one ordered transport.
	Concurrency int32
	// TimeoutMillis bounds a single probe. This is the number that decides
	// whether a stalled flow reports as "hung" or as an error, so it should be
	// well above normal latency and well below a browser's own timeout.
	TimeoutMillis int64

	// RepeatCount runs the whole list this many times. Repeats after the first
	// exercise warm paths, where the first is cold.
	RepeatCount int32

	IncludeDns      bool
	IncludeHttp     bool
	IncludeDownload bool

	// DownloadByteCount is the size fetched by the download probe.
	DownloadByteCount int64
}

// Bounds for a probe config.
//
// The config is untrusted input at the point it is used, not a local literal:
// on desktop it is composed in the ui process and applied in the privileged
// service, so every field crosses a process boundary before anything reads
// it. Concurrency is a goroutine count -- `ProbeSuiteConfig{Concurrency:
// 1<<30}` is a billion goroutines inside the service -- RepeatCount
// multiplies the job list, and DownloadByteCount is bytes pulled over the
// tunnel by every repeat.
//
// Only the ceilings are enforced for Concurrency and RepeatCount, since
// `buildProbeJobs`/`run` already raise a low value to 1. DownloadByteCount
// deliberately has no floor: `buildProbeJobs` treats a non-positive value as
// "no download probe", which is a meaningful choice rather than an error.
const (
	probeMaxConcurrency = 64
	probeMaxRepeatCount = 100

	// a zero timeout is "no deadline", which is how a stalled probe becomes a
	// goroutine that never returns
	probeMinTimeoutMillis = 100
	probeMaxTimeoutMillis = 300_000

	probeMaxDownloadByteCount = 256 << 20
)

// normalizeProbeSuiteConfig resolves a nil config to the default and bounds
// every field. It returns a COPY, so a caller that keeps mutating its struct
// after starting a run cannot change the run in flight.
func normalizeProbeSuiteConfig(config *ProbeSuiteConfig, log connect.Logger) *ProbeSuiteConfig {
	if config == nil {
		return GetDefaultProbeSuiteConfig()
	}

	normalized := *config

	clamp := func(name string, value int64, low int64, high int64) int64 {
		bounded := min(max(value, low), high)
		if bounded != value && log != nil {
			log.Infof("[probe]%s %d out of range, clamped to %d\n", name, value, bounded)
		}
		return bounded
	}
	// ceiling only: `run` and `buildProbeJobs` already raise a low value to 1,
	// and a floor here would turn "0 means whatever the runner decides" into a
	// second spelling of the same thing
	ceiling := func(name string, value int64, high int64) int64 {
		return clamp(name, value, min(value, high), high)
	}

	normalized.Concurrency = int32(ceiling("concurrency", int64(normalized.Concurrency), probeMaxConcurrency))
	normalized.RepeatCount = int32(ceiling("repeat_count", int64(normalized.RepeatCount), probeMaxRepeatCount))
	normalized.TimeoutMillis = clamp("timeout_millis", normalized.TimeoutMillis, probeMinTimeoutMillis, probeMaxTimeoutMillis)
	normalized.DownloadByteCount = ceiling("download_byte_count", normalized.DownloadByteCount, probeMaxDownloadByteCount)

	return &normalized
}

// Named Get* to match the other bound defaults (GetDefaultDnsResolverSettings),
// which is how gomobile surfaces them as Sdk.getDefault... on the app side.
func GetDefaultProbeSuiteConfig() *ProbeSuiteConfig {
	return &ProbeSuiteConfig{
		Concurrency:       4,
		TimeoutMillis:     15_000,
		RepeatCount:       1,
		IncludeDns:        true,
		IncludeHttp:       true,
		IncludeDownload:   true,
		DownloadByteCount: 1 << 20,
	}
}

// probeHttpTargets are small, widely-anycast, and operated by parties who
// publish endpoints for exactly this purpose. They are deliberately spread
// across operators: a list from one provider measures the route to that
// provider, not the tunnel.
var probeHttpTargets = []string{
	"https://www.cloudflare.com/cdn-cgi/trace",
	"https://www.google.com/generate_204",
	"https://example.com/",
	"https://www.wikipedia.org/",
}

// probeDnsTargets are resolved fresh. Names are chosen to be unlikely to sit
// in a cache from ordinary browsing, since a cache hit measures nothing.
var probeDnsTargets = []string{
	"one.one.one.one",
	"dns.google",
	"example.com",
}

// probeDownloadUrl serves arbitrary sizes, so throughput is measured over a
// controlled transfer rather than whatever a page happens to weigh.
const probeDownloadUrl = "https://speed.cloudflare.com/__down?bytes=%d"

// probeSuite owns one run. Only one runs at a time per device: concurrent runs
// would contend for the same exits and neither result would mean anything.
type probeSuite struct {
	stateLock sync.Mutex
	running   bool
	results   []*ProbeResult
	cancel    context.CancelFunc
}

// probeHarness is the tun pumped into the live device, plus the http client
// that dials through it.
type probeHarness struct {
	tun        *connect.Tun
	httpClient *http.Client
	unsub      func()
	cancel     context.CancelFunc
	localAddrs []netip.Addr
}

func newProbeHarness(ctx context.Context, device *DeviceLocal) (*probeHarness, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	tun, err := connect.CreateTun(cancelCtx, connect.DefaultTunSettings())
	if err != nil {
		cancel()
		return nil, err
	}

	localAddrs := tun.LocalAddresses()

	// egress: the tun's stack emits packets, the device sends them over the
	// same exits the browser is using
	go func() {
		defer cancel()
		for {
			packet, err := tun.Read()
			if err != nil {
				return
			}
			select {
			case <-cancelCtx.Done():
				return
			default:
			}
			device.SendPacket(packet, int32(len(packet)))
		}
	}()

	// ingress: the device fans received packets out to every subscriber, so
	// take the ones addressed to this tun. The real tunnel is also a
	// subscriber and will see these, but its interface does not hold this
	// address so the OS drops them.
	unsub := device.AddReceivePacketCallback(func(
		source connect.TransferPath,
		provideMode protocol.ProvideMode,
		ipPath *connect.IpPath,
		packet []byte,
	) {
		if !probeAddrMatches(localAddrs, ipPath.DestinationIp) {
			return
		}
		tun.Write(packet)
	})

	harness := &probeHarness{
		tun:        tun,
		unsub:      unsub,
		cancel:     cancel,
		localAddrs: localAddrs,
	}

	harness.httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: tun.DialContext,
			// every probe measures a cold connection. reuse is worth having in
			// production and is tracked separately as its own candidate, but
			// here it would silently turn most probes into a no-op.
			DisableKeepAlives: true,
		},
		// redirects would time a different url than the one named in the result
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return harness, nil
}

func probeAddrMatches(localAddrs []netip.Addr, ip net.IP) bool {
	if len(ip) == 0 {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, localAddr := range localAddrs {
		if localAddr.Unmap() == addr {
			return true
		}
	}
	return false
}

func (self *probeHarness) close() {
	self.unsub()
	self.cancel()
	self.tun.Close()
}

// httpProbe times one request, broken into phases so a slow result says which
// part was slow. A new destination pays dns, connect, and ttfb; a stalled exit
// shows a normal connect and a ttfb that never arrives.
func (self *probeHarness) httpProbe(
	ctx context.Context,
	name string,
	url string,
	kind string,
	timeout time.Duration,
	startOffset time.Duration,
) *ProbeResult {
	result := &ProbeResult{
		Name:              name,
		Kind:              kind,
		DnsMillis:         -1,
		ConnectMillis:     -1,
		TtfbMillis:        -1,
		StartOffsetMillis: startOffset.Milliseconds(),
	}

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var dnsStart, connectStart time.Time
	start := time.Now()

	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			result.DnsMillis = time.Since(dnsStart).Milliseconds()
		},
		ConnectStart: func(string, string) { connectStart = time.Now() },
		ConnectDone: func(network string, addr string, err error) {
			if err == nil {
				result.ConnectMillis = time.Since(connectStart).Milliseconds()
			}
		},
		GotFirstResponseByte: func() {
			result.TtfbMillis = time.Since(start).Milliseconds()
		},
	}

	request, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(requestCtx, trace),
		http.MethodGet,
		url,
		nil,
	)
	if err != nil {
		result.Error = err.Error()
		result.TotalMillis = time.Since(start).Milliseconds()
		return result
	}
	request.Header.Set("User-Agent", probeUserAgent)

	response, err := self.httpClient.Do(request)
	if err != nil {
		result.Error = err.Error()
		result.TotalMillis = time.Since(start).Milliseconds()
		return result
	}
	defer response.Body.Close()

	byteCount, err := io.Copy(io.Discard, response.Body)
	result.ByteCount = byteCount
	result.TotalMillis = time.Since(start).Milliseconds()
	if err != nil {
		// a body that starts and then stops is the signature of an exit dying
		// mid-transfer, and is a different failure from one that never started
		result.Error = err.Error()
		return result
	}

	result.Ok = true
	return result
}

// dnsProbe resolves a name through the tunnel's resolver.
func (self *probeHarness) dnsProbe(
	ctx context.Context,
	name string,
	timeout time.Duration,
	startOffset time.Duration,
) *ProbeResult {
	result := &ProbeResult{
		Name:              name,
		Kind:              "dns",
		DnsMillis:         -1,
		ConnectMillis:     -1,
		TtfbMillis:        -1,
		StartOffsetMillis: startOffset.Milliseconds(),
	}

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return self.tun.DialContext(ctx, network, address)
		},
	}

	start := time.Now()
	addrs, err := resolver.LookupHost(requestCtx, name)
	result.TotalMillis = time.Since(start).Milliseconds()
	result.DnsMillis = result.TotalMillis
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.ByteCount = int64(len(addrs))
	result.Ok = true
	return result
}

// probeJob is one unit of work, resolved before the run starts so both halves
// of an A/B do identical work in an identical order.
type probeJob struct {
	name string
	kind string
	url  string
}

func buildProbeJobs(config *ProbeSuiteConfig) []*probeJob {
	jobs := []*probeJob{}
	repeats := max(int(config.RepeatCount), 1)

	for range repeats {
		if config.IncludeDns {
			for _, target := range probeDnsTargets {
				jobs = append(jobs, &probeJob{name: target, kind: "dns"})
			}
		}
		if config.IncludeHttp {
			for _, target := range probeHttpTargets {
				jobs = append(jobs, &probeJob{name: target, kind: "http", url: target})
			}
		}
		if config.IncludeDownload && 0 < config.DownloadByteCount {
			jobs = append(jobs, &probeJob{
				name: fmt.Sprintf("download %dKiB", config.DownloadByteCount/1024),
				kind: "download",
				url:  fmt.Sprintf(probeDownloadUrl, config.DownloadByteCount),
			})
		}
	}
	return jobs
}

// run executes the jobs and records results as they land, so a run can be read
// while it is still going -- which matters, because the interesting moment is
// the one right after an exit is stalled mid-run.
func (self *probeSuite) run(
	ctx context.Context,
	device *DeviceLocal,
	config *ProbeSuiteConfig,
) {
	defer func() {
		self.stateLock.Lock()
		self.running = false
		self.stateLock.Unlock()
	}()

	harness, err := newProbeHarness(ctx, device)
	if err != nil {
		self.appendResult(&ProbeResult{
			Name:  "harness",
			Kind:  "setup",
			Error: fmt.Sprintf("could not start the probe tunnel: %s", err),
		})
		return
	}
	defer harness.close()

	jobs := buildProbeJobs(config)
	timeout := millis(config.TimeoutMillis)
	concurrency := max(int(config.Concurrency), 1)

	start := time.Now()
	jobChan := make(chan *probeJob)
	wg := sync.WaitGroup{}

	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				offset := time.Since(start)
				var result *ProbeResult
				switch job.kind {
				case "dns":
					result = harness.dnsProbe(ctx, job.name, timeout, offset)
				default:
					result = harness.httpProbe(ctx, job.name, job.url, job.kind, timeout, offset)
				}
				self.appendResult(result)
			}
		}()
	}

	for _, job := range jobs {
		select {
		case <-ctx.Done():
			close(jobChan)
			wg.Wait()
			return
		case jobChan <- job:
		}
	}
	close(jobChan)
	wg.Wait()
}

func (self *probeSuite) appendResult(result *ProbeResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.results = append(self.results, result)
}

// ProbeSuiteRunning reports whether a run is in progress. The UI polls this
// rather than blocking, since a run outlives any single call.
func (self *DeviceLocal) ProbeSuiteRunning() bool {
	suite := self.probeSuiteState
	if suite == nil {
		return false
	}
	suite.stateLock.Lock()
	defer suite.stateLock.Unlock()
	return suite.running
}

// StartProbeSuite begins a run and returns immediately. Returns false if one
// is already running -- two concurrent runs would contend for the same exits
// and neither result would mean anything.
//
// A nil config means the default. This is enforced HERE rather than at any
// one caller because the config reaches `buildProbeJobs` on a spawned
// goroutine: a nil deref there is not recoverable by the cgo boundary's
// recover, so it aborts the whole process -- on Windows, the privileged
// service. `urnet_device_local_start_probe_suite(handle, NULL)` and gomobile's
// `startProbeSuite(null)` both reach this directly, with no rpc in between.
func (self *DeviceLocal) StartProbeSuite(config *ProbeSuiteConfig) bool {
	suite := self.probeSuiteState
	if suite == nil {
		return false
	}
	config = normalizeProbeSuiteConfig(config, self.log)

	suite.stateLock.Lock()
	if suite.running {
		suite.stateLock.Unlock()
		return false
	}
	runCtx, cancel := context.WithCancel(self.ctx)
	suite.running = true
	suite.results = []*ProbeResult{}
	suite.cancel = cancel
	suite.stateLock.Unlock()

	go suite.run(runCtx, self, config)
	return true
}

// StopProbeSuite cancels a run in progress. Results collected so far are kept.
func (self *DeviceLocal) StopProbeSuite() {
	suite := self.probeSuiteState
	if suite == nil {
		return
	}
	suite.stateLock.Lock()
	cancel := suite.cancel
	suite.stateLock.Unlock()
	if cancel != nil {
		cancel()
	}
}

// GetProbeResults reports what has completed so far. Safe to call during a
// run, which is the point -- stalling an exit mid-run and watching the next
// results land is the measurement.
func (self *DeviceLocal) GetProbeResults() *ProbeResultList {
	results := NewProbeResultList()
	suite := self.probeSuiteState
	if suite == nil {
		return results
	}

	suite.stateLock.Lock()
	defer suite.stateLock.Unlock()
	for _, result := range suite.results {
		results.Add(result)
	}
	return results
}
