package sdk

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	// "hash/fnv"
	"encoding/json"
	"flag"
	"math"

	// "math/big"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	// "strings"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"golang.org/x/crypto/nacl/box"
)

// note: publicly exported types must be fully contained in the `client` package tree
// the `gomobile` native interface compiler won't be able to map types otherwise
// a number of types (struct, function, interface) are redefined in `client`,
// somtimes in a simplified way, and then internally converted back to the native type
// examples:
// - fixed primitive arrays are not exportable. Use slices instead.
// - raw structs are not exportable. Use pointers to structs instead.
//   e.g. Id that is to be exported needs to be *Id
// - redefined primitive types are not exportable. Use type aliases instead.
// - arrays of structs are not exportable. See https://github.com/golang/go/issues/13445
//   use the "ExportableList" workaround from `gomobile.go`
// - exported names start with Get* and Set* to be compatible with target language features
//
// additionally, the entire bringyour.com/bringyour tree cannot be used because it pulls in the
// `warp` environment expectations, which is not compatible with the client lib

func init() {
	// Version is populated by the linker before package initialization. Stamp
	// the shared Connect core once so a device acting as an exit publishes the
	// actual SDK/app build rather than an empty provider identity.
	stampConnectBuildVersion()

	// gc pacing: the go soft memory limit (see SetMemoryLimit) is the
	// footprint backstop; gogc paces how often the collector runs below it.
	// The 24-MiB profile uses 25: the measured 20-MiB candidate's value of 10
	// held more than two MiB of unused headroom while collecting roughly every
	// 2.5 seconds during a low-throughput transfer, while 50 let a stalled H3
	// page reach 29.95 MiB. The aggregate packet gate, quiet reclaim, and
	// 32-MiB soft limit remain the burst backstops. Android and iOS deliberately
	// use the same value so the measurable Android surrogate does not hide iOS
	// allocator float. Desktop/server retains the runtime default.
	debug.SetGCPercent(gcPercentForPlatform(runtime.GOOS))

	initGlog()
}

func stampConnectBuildVersion() {
	connect.SetBuildVersion(Version)
}

func gcPercentForPlatform(goos string) int {
	switch goos {
	case "android", "ios":
		return 25
	default:
		return 100
	}
}

func initGlog() {
	// flag.Set("logtostderr", "true")
	flag.Set("alsologtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to
	// stdout (redirectStderrForPlatform is a no-op on other platforms —
	// desktop consumers like sim-latency need stdout clean for data output)
	redirectStderrForPlatform()
}

func clearOldLogs(logDir string) {

	// get all files that contain .log.INFO in the string
	files, err := os.ReadDir(logDir)
	if err != nil {
		glog.Errorf("Failed to read log directory %q: %v", logDir, err)
		return
	}

	logPaths := []string{}

	for _, file := range files {
		name := file.Name()
		if !file.IsDir() && (bytes.Contains([]byte(name), []byte(".log.INFO")) || bytes.Contains([]byte(name), []byte(".log.WARNING")) || bytes.Contains([]byte(name), []byte(".log.ERROR")) || bytes.Contains([]byte(name), []byte(".log.FATAL"))) {
			fullPath := logDir + "/" + name
			logPaths = append(logPaths, fullPath)
		}
	}

	// order by modification time, oldest first
	type fileInfo struct {
		path    string
		modTime int64
	}

	fileInfos := []fileInfo{}

	for _, path := range logPaths {
		info, err := os.Stat(path)
		if err != nil {
			glog.Errorf("Failed to stat log file %q: %v", path, err)
			continue
		}
		fileInfos = append(fileInfos, fileInfo{path: path, modTime: info.ModTime().Unix()})
	}

	// sort by modTime
	for i := 0; i < len(fileInfos)-1; i++ {
		for j := i + 1; j < len(fileInfos); j++ {
			if fileInfos[i].modTime > fileInfos[j].modTime {
				fileInfos[i], fileInfos[j] = fileInfos[j], fileInfos[i]
			}
		}
	}

	glog.Infof("Found %d log files in %q", len(fileInfos), logDir)

	// keep only the 4 most recent logs
	if len(fileInfos) > 4 {
		toDelete := fileInfos[:len(fileInfos)-4]
		for _, fi := range toDelete {
			err := os.Remove(fi.path)
			if err != nil {
				glog.Errorf("Failed to remove old log file %q: %v", fi.path, err)
			} else {
				glog.Infof("Removed old log file %q", fi.path)
			}
		}
	}

}

func GetLogDir() string {
	if f := flag.Lookup("log_dir"); f != nil {
		return f.Value.String()
	}
	return ""
}

func FlushGlog() {
	glog.Flush()
}

func SetLogDir(logDir string) error {

	glog.SetMaxLogSize(1024 * 1024 * 16)
	err := glog.SetLogDir(logDir)
	if err != nil {
		glog.Infof("SetLogDir to %q failed: %v", logDir, err)
	}
	glog.Infof("New glog initialized")
	clearOldLogs(logDir)

	return err
}

// memory target ratio: how SetMemoryLimit divides the process budget into
// the global message pool bounds, in parts of `memoryTargetRatioParts`. at
// the reference 34 MB budget this lands on 12 MB packet pool / 2 MB large
// object pools. Pool retention is live heap against the go soft memory
// limit, so oversized pool caps squeeze the collector into assist mode near
// the limit (measured as an ios throughput regression at 22 MB of caps) —
// the caps only need to cover the in-flight high-water. The remaining 20
// parts are the reference per-device 20 MB memory target (split dns 2 :
// client 14 : provider 4 inside the device) — each device's target is set
// explicitly where the device is created (see
// DeviceLocalSettings.MemoryTargetByteCount and
// NewDeviceLocalWithMemoryTarget), not by this call.
const (
	memoryTargetRatioPacketPool      = 12
	memoryTargetRatioLargeObjectPool = 2
	memoryTargetRatioParts           = 34
)

// Derives free-list capacities from the process limit while applying the
// tighter mobile returned-buffer ceiling. Servers retain the historical
// proportional sizing; live/in-flight allocations are not governed here.
func messagePoolMemoryTargetsForPlatform(
	limit int64,
	mobile bool,
) (packetPoolByteCount int64, largeObjectPoolByteCount int64) {
	packetPoolByteCount = limit * memoryTargetRatioPacketPool / memoryTargetRatioParts
	largeObjectPoolByteCount = limit * memoryTargetRatioLargeObjectPool / memoryTargetRatioParts
	if mobile {
		packetPoolByteCount = min(
			packetPoolByteCount,
			int64(mobilePacketPoolCapacityByteCount),
		)
		largeObjectPoolByteCount = min(
			largeObjectPoolByteCount,
			int64(mobileLargeObjectPoolCapacityByteCount),
		)
	}
	return
}

// SetMemoryLimit tunes the sdk to a process memory budget. the app-facing
// process-level knob:
//   - bounds the global message pool free lists by ratio (packet pool 12 :
//     large object pools 2, of 34 parts) via SetMessagePoolMemoryTargets
//   - scales the remaining memory-dominant connect defaults (receive
//     windows, socket buffers, cache bounds) for objects constructed after
//     this call
//   - sets the go runtime soft memory limit
//
// call this at process start. It deliberately does NOT size per-device
// memory: each DeviceLocal's target is passed explicitly where the device
// is created, so a multi-device process bounds every device independently.
func SetMemoryLimit(limit int64) {
	packetPoolByteCount, largeObjectPoolByteCount :=
		messagePoolMemoryTargetsForPlatform(limit, mobileRuntime())
	SetMessagePoolMemoryTargets(packetPoolByteCount, largeObjectPoolByteCount)
	// Pre-warm a bounded part of the packet class so the first traffic burst
	// skips the cold allocation storm. Mobile retains 512 KiB; desktop/server
	// preserve the historical 1 MiB.
	// Startup only — the pressure path (FreeMemory) deliberately leaves pools
	// cold.
	if mobileRuntime() {
		connect.WarmMessagePoolsTo(mobilePacketPoolWarmByteCount)
	} else {
		connect.WarmMessagePools()
	}
	connect.SetMemoryBudget(limit)
	debug.SetMemoryLimit(limit)
	startMobileIdleMemoryTrimmer()
}

// SetMessagePoolMemoryTargets bounds the global message pool free lists:
// packetPoolByteCount is split between the 256-byte small/control and 2048-byte
// full-MTU packet classes, and largeObjectPoolByteCount is split evenly among
// the larger size classes.
// The pools are the process-global complement to the per-device memory
// target (DeviceLocalSettings.MemoryTargetByteCount). Applies live.
func SetMessagePoolMemoryTargets(
	packetPoolByteCount int64,
	largeObjectPoolByteCount int64,
) {
	connect.ResizeMessagePools(packetPoolByteCount, largeObjectPoolByteCount)
}

// SetEgressInterfaceIndex binds the sdk's outbound sockets to the given
// physical network interface indices (IPv4 and IPv6), so that when this process
// provides a VPN tunnel its own platform and provider connections do not loop
// back into that tunnel. Pass 0 for a family to leave it unbound. This is the
// Windows self-exclusion mechanism (R1) and the macOS controlled-peer
// acceptance mechanism; it is a no-op on other platforms, where the OS handles
// self-exclusion. The Windows service updates these on every network change.
func SetEgressInterfaceIndex(index4 int, index6 int) {
	connect.SetEgressInterfaceIndex(uint32(index4), uint32(index6))
}

// FreeMemory drops recoverable memory in response to host memory pressure
func FreeMemory() {
	startTime := time.Now()
	totalByteCountBefore := runtimeTotalByteCount()
	// drop recoverable caches (resolver caches, pooled DoH connections, affinity maps)
	// before releasing the pools and returning free spans to the OS
	connect.ShedMemory()
	connect.ClearMessagePools()
	debug.FreeOSMemory()
	glog.Infof(
		"[mem]free memory %.1fmib -> %.1fmib (%dms)",
		float64(totalByteCountBefore)/float64(1024*1024),
		float64(runtimeTotalByteCount())/float64(1024*1024),
		time.Since(startTime)/time.Millisecond,
	)
}

// TrimMemory rebuilds burst-sized message-pool free lists as their warm reuse
// set and returns the old spans to the OS. Clearing before collection and
// warming afterward matters: retaining an arbitrary subset of burst buffers
// can pin sparsely occupied allocator spans even when their byte sum is small.
// Unlike FreeMemory, this preserves resolver/connection/affinity caches and
// every pool's configured capacity. Hosts may use it after a verified
// traffic-quiescent interval; active and in-flight buffers are never affected.
func TrimMemory() {
	trimMemory(true)
}

// A forced collection has a latency and battery cost. Automatic maintenance
// therefore rebuilds allocator spans only after at least one additional MiB
// accumulated above the warm set. Smaller idle refills are still pruned from
// the free lists, but normal GC can reclaim them; explicit host pressure always
// forces release.
const automaticIdleMemoryRebuildMinDroppedByteCount ByteCount = 1024 * 1024

// trimMemory returns the bytes dropped by a material pool rebuild. Automatic
// idle maintenance skips the forced collection when the pools are already warm
// or only trivially above it; an explicit host request still forces release so
// it has deterministic pressure semantics.
func trimMemory(force bool) ByteCount {
	warmByteCount := ByteCount(1024 * 1024)
	if mobileRuntime() {
		warmByteCount = mobilePacketPoolWarmByteCount
	}
	return rebuildMessagePools(force, warmByteCount)
}

func rebuildMessagePools(force bool, warmByteCount ByteCount) ByteCount {
	startTime := time.Now()
	totalByteCountBefore := runtimeTotalByteCount()
	// This first decay is a cheap no-op test for automatic maintenance. If an
	// idle high-water exists, clear all remaining free-list references so the
	// collector can release whole spans rather than leaving sparse survivors.
	droppedByteCount := connect.TrimMessagePoolsTo(warmByteCount)
	if !force && droppedByteCount < automaticIdleMemoryRebuildMinDroppedByteCount {
		return 0
	}
	retainedBefore := connect.GetMessagePoolAggregateStats().RetainedByteCount
	connect.ClearMessagePools()
	debug.FreeOSMemory()
	connect.WarmMessagePoolsTo(warmByteCount)
	retainedAfter := connect.GetMessagePoolAggregateStats().RetainedByteCount
	droppedByteCount += max(ByteCount(0), retainedBefore-retainedAfter)
	glog.Infof(
		"[mem]rebuild idle pools %.1fmib -> %.1fmib dropped=%.1fmib (%dms)",
		float64(totalByteCountBefore)/float64(1024*1024),
		float64(runtimeTotalByteCount())/float64(1024*1024),
		float64(droppedByteCount)/float64(1024*1024),
		time.Since(startTime)/time.Millisecond,
	)
	return droppedByteCount
}

func MessagePoolGet(n int) []byte {
	b := connect.MessagePoolGet(n)
	// return b[:cap(b)]
	return b
}

func MessagePoolGetRaw(n int) []byte {
	b := connect.MessagePoolGet(n)
	return b[:cap(b)]
}

func MessagePoolReturn(b []byte) {
	connect.MessagePoolReturn(b)
}

// this value is set via the linker, e.g.
// -ldflags "-X github.com/urnetwork/sdk.Version=$WARP_VERSION-$WARP_VERSION_CODE"
//
// MUST stay a `var`. `-X` only sets string *variables* declared uninitialized
// or initialized to a constant expression — against a `const` it is silently a
// no-op, and every build reports "". This was a const until 2026-08-05, so
// `urnet_version()` had always returned the empty string despite the flag being
// passed. Verified by rebuilding with -X set and reading it back.
//
// The var/const distinction also changes the SHAPE of the gomobile binding.
// gomobile binds a package-level var as an accessor pair rather than a
// constant, so the bound call sites had to change with it:
//
//	as const:  apple `SdkVersion`      · android `Sdk.Version`
//	as var:    apple `SdkVersion()`    · android `Sdk.getVersion()`
//
// Do NOT add a hand-written `func GetVersion()` alongside this — gobind already
// emits `Java_com_bringyour_sdk_Sdk_getVersion` for the var, and the extra func
// collides with it ("redefinition of ...Sdk_getVersion") and breaks
// build_android. The cgo C ABI is unaffected either way: exports_core.go reads
// the var directly at runtime.
var Version string = ""

type Id struct {
	id [16]byte
	// store this on the object to support gomobile "equals" and "hashCode"
	IdStr string
}

func newId(id [16]byte) *Id {
	return &Id{
		id:    id,
		IdStr: encodeUuid(id),
	}
}

func NewId() *Id {
	return newId(connect.NewId())
}

func IdFromBytes(idBytes []byte) (*Id, error) {
	if len(idBytes) != 16 {
		return nil, fmt.Errorf("Id bytes must be length 16")
	}
	return newId([16]byte(idBytes)), nil
}

func RequireIdFromBytes(idBytes []byte) *Id {
	id, err := IdFromBytes(idBytes)
	if err != nil {
		panic(err)
	}
	return id
}

func ParseId(src string) (*Id, error) {
	dst, err := parseUuid(src)
	if err != nil {
		return nil, err
	}
	return newId(dst), nil
}

func (self *Id) Bytes() []byte {
	return self.id[:]
}

func (self *Id) String() string {
	return self.IdStr
}

/*
func (self *Id) StringForConsole() string {
	// the macOS console shows <private> for ids which makes them hard to debug
	// https://mjtsai.com/blog/2019/11/21/catalinas-log-cant-be-unprivatised/
	// note this can be decoded with `echo -n "obase=16;<id>" | bc | xxd -r -p`
	i := &big.Int{}
	i.SetBytes([]byte(self.String()))
	return i.Text(10)
}
*/

func (self *Id) Cmp(b *Id) int {
	for i, v := range self.id {
		if v < b.id[i] {
			return -1
		}
		if b.id[i] < v {
			return 1
		}
	}
	return 0
}

func (self *Id) toConnectId() connect.Id {
	return self.id
}

func (self *Id) MarshalJSON() ([]byte, error) {
	var buf [16]byte
	copy(buf[0:16], self.id[0:16])
	var buff bytes.Buffer
	buff.WriteByte('"')
	buff.WriteString(encodeUuid(buf))
	buff.WriteByte('"')
	b := buff.Bytes()
	// gmLog("MARSHAL ID TO: %s", string(b))
	return b, nil
}

func (self *Id) UnmarshalJSON(src []byte) error {
	if len(src) != 38 {
		return fmt.Errorf("invalid length for UUID: %v", len(src))
	}
	buf, err := parseUuid(string(src[1 : len(src)-1]))
	if err != nil {
		return err
	}
	self.id = buf
	self.IdStr = encodeUuid(buf)
	return nil
}

// Android support

// func (self *Id) IdEquals(b *Id) bool {
// 	if b == nil {
// 		return false
// 	}
// 	return self.id == b.id
// }

// func (self *Id) IdHashCode() int32 {
// 	h := fnv.New32()
// 	h.Write(self.id[:])
// 	return int32(h.Sum32())
// }

// parseUuid converts a string UUID in standard form to a byte array.
func parseUuid(src string) (dst [16]byte, err error) {
	switch len(src) {
	case 36:
		src = src[0:8] + src[9:13] + src[14:18] + src[19:23] + src[24:]
	case 32:
		// dashes already stripped, assume valid
	default:
		// assume invalid.
		return dst, fmt.Errorf("cannot parse UUID %v", src)
	}

	buf, err := hex.DecodeString(src)
	if err != nil {
		return dst, err
	}

	copy(dst[:], buf)
	return dst, err
}

func encodeUuid(src [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", src[0:4], src[4:6], src[6:8], src[8:10], src[10:16])
}

type TransferPath struct {
	SourceId      *Id
	DestinationId *Id
	StreamId      *Id
}

func NewTransferPath(sourceId *Id, destinationId *Id, streamId *Id) *TransferPath {
	return &TransferPath{
		SourceId:      sourceId,
		DestinationId: destinationId,
		StreamId:      streamId,
	}
}

func fromConnect(path connect.TransferPath) *TransferPath {
	return &TransferPath{
		SourceId:      newId(path.SourceId),
		DestinationId: newId(path.DestinationId),
		StreamId:      newId(path.StreamId),
	}
}

func (self *TransferPath) toConnect() connect.TransferPath {
	path := connect.TransferPath{}
	if self.SourceId != nil {
		path.SourceId = connect.Id(self.SourceId.id)
	}
	if self.DestinationId != nil {
		path.DestinationId = connect.Id(self.DestinationId.id)
	}
	if self.StreamId != nil {
		path.StreamId = connect.Id(self.StreamId.id)
	}
	return path
}

type ProvideMode = int

const (
	ProvideModeNone             ProvideMode = ProvideMode(protocol.ProvideMode_None)
	ProvideModeNetwork          ProvideMode = ProvideMode(protocol.ProvideMode_Network)
	ProvideModeFriendsAndFamily ProvideMode = ProvideMode(protocol.ProvideMode_FriendsAndFamily)
	ProvideModePublic           ProvideMode = ProvideMode(protocol.ProvideMode_Public)
	ProvideModeStream           ProvideMode = ProvideMode(protocol.ProvideMode_Stream)
)

type LocationType = string

const (
	LocationTypeCountry LocationType = "country"
	LocationTypeRegion  LocationType = "region"
	LocationTypeCity    LocationType = "city"
)

type ProvideControlMode = string

const (
	ProvideControlModeNever  ProvideControlMode = "never"
	ProvideControlModeAlways ProvideControlMode = "always"
	ProvideControlModeAuto   ProvideControlMode = "auto"
	ProvideControlModeManual ProvideControlMode = "manual"
	// the private provider: the provider is always on, but provides ONLY to
	// same-network peers (Network provide mode) — never publicly
	ProvideControlModeNetwork ProvideControlMode = "network"
)

type ProvideNetworkMode = string

const (
	ProvideNetworkModeWiFi ProvideNetworkMode = "wifi"
	ProvideNetworkModeAll  ProvideNetworkMode = "all" // allow providing on wifi and cell networks
)

type ByteCount = int64

type NanoCents = int64

func UsdToNanoCents(usd float64) NanoCents {
	return NanoCents(math.Round(usd * float64(1000000000)))
}

func NanoCentsToUsd(nanoCents NanoCents) float64 {
	return float64(nanoCents) / float64(1000000000)
}

type NanoPoints = int64

// 1 point = 1_000_000 nano points

func PointsToNanoPoints(points float64) NanoPoints {
	return NanoPoints(math.Round(float64(points) * 1_000_000))
}

func NanoPointsToPoints(nanoPoints NanoPoints) float64 {
	return math.Round(float64(nanoPoints) / 1_000_000)
}

// merged location and location group
type ConnectLocation struct {
	ConnectLocationId *ConnectLocationId `json:"connect_location_id,omitempty"`
	Name              string             `json:"name,omitempty"`

	ProviderCount int32 `json:"provider_count,omitempty"`
	Promoted      bool  `json:"promoted,omitempty"`
	MatchDistance int32 `json:"match_distance,omitempty"`

	LocationType LocationType `json:"location_type,omitempty"`

	City        string `json:"city,omitempty"`
	Region      string `json:"region,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`

	CityLocationId    *Id `json:"city_location_id,omitempty"`
	RegionLocationId  *Id `json:"region_location_id,omitempty"`
	CountryLocationId *Id `json:"country_location_id,omitempty"`

	Stable        bool `json:"stable"`
	StrongPrivacy bool `json:"strong_privacy"`

	// NetworkPeer marks this location as one of the user's own trusted network
	// peers (selected from the network peers list). Such a connection egresses
	// under ProvideMode_Network. This is explicit state set by the caller — a
	// fixed client id alone does not imply a trusted peer (it can be a public exit).
	NetworkPeer bool `json:"network_peer,omitempty"`
}

func (self *ConnectLocation) IsGroup() bool {
	return self.ConnectLocationId.IsGroup()
}

func (self *ConnectLocation) IsDevice() bool {
	return self.ConnectLocationId.IsDevice()
}

func (self *ConnectLocation) ToRegion() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name:              self.Region,

		ProviderCount: self.ProviderCount,
		Promoted:      false,
		MatchDistance: 0,

		LocationType: LocationTypeRegion,

		City:        "",
		Region:      self.Region,
		Country:     self.Country,
		CountryCode: self.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  self.RegionLocationId,
		CountryLocationId: self.CountryLocationId,
	}
}

func (self *ConnectLocation) ToCountry() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name:              self.Country,

		ProviderCount: self.ProviderCount,
		Promoted:      false,
		MatchDistance: 0,

		LocationType: LocationTypeCountry,

		City:        "",
		Region:      "",
		Country:     self.Country,
		CountryCode: self.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  nil,
		CountryLocationId: self.CountryLocationId,
	}
}

func (self *ConnectLocation) Equals(b *ConnectLocation) bool {
	if b == nil {
		return false
	}
	if self.ConnectLocationId == nil || b.ConnectLocationId == nil {
		return self.ConnectLocationId == nil && b.ConnectLocationId == nil
	}
	return self.ConnectLocationId.Cmp(b.ConnectLocationId) == 0
}

// merged location and location group
type ConnectLocationId struct {
	// if set, the location is a direct connection to another device
	ClientId        *Id  `json:"client_id,omitempty"`
	LocationId      *Id  `json:"location_id,omitempty"`
	LocationGroupId *Id  `json:"location_group_id,omitempty"`
	BestAvailable   bool `json:"best_available,omitempty"`
}

func (self *ConnectLocationId) IsGroup() bool {
	return self.LocationGroupId != nil
}

func (self *ConnectLocationId) IsDevice() bool {
	return self.ClientId != nil
}

func (self *ConnectLocationId) Cmp(b *ConnectLocationId) int {
	// - direct
	// - group
	if b == nil {
		return -1
	}
	if self.ClientId != nil && b.ClientId != nil {
		if c := self.ClientId.Cmp(b.ClientId); c != 0 {
			return c
		}
	} else if self.ClientId != nil {
		return -1
	} else if b.ClientId != nil {
		return 1
	}

	if self.LocationGroupId != nil && b.LocationGroupId != nil {
		if c := self.LocationGroupId.Cmp(b.LocationGroupId); c != 0 {
			return c
		}
	} else if self.LocationGroupId != nil {
		return -1
	} else if b.LocationGroupId != nil {
		return 1
	}

	if self.LocationId != nil && b.LocationId != nil {
		if c := self.LocationId.Cmp(b.LocationId); c != 0 {
			return c
		}
	} else if self.LocationId != nil {
		return -1
	} else if b.LocationId != nil {
		return 1
	}

	if self.BestAvailable != b.BestAvailable {
		if self.BestAvailable {
			return -1
		} else {
			return 1
		}
	}

	return 0
}

func (self *ConnectLocationId) String() string {
	jsonBytes, err := json.Marshal(self)
	if err != nil {
		panic(err)
	}
	return string(jsonBytes)
}

type ProvideSecretKey struct {
	ProvideMode      ProvideMode `json:"provide_mode"`
	ProvideSecretKey string      `json:"provide_secret_key"`
}

type WindowType = string

const (
	// no fixed window type. A nil performance profile and window type auto
	// mean the same thing: traffic balances across the window types and the
	// window size settings are ignored.
	WindowTypeAuto    WindowType = "auto"
	WindowTypeQuality WindowType = "quality"
	WindowTypeSpeed   WindowType = "speed"
)

// a nil profile, or a profile with window type auto (or unset), uses the
// default "auto" mode. The orthogonal settings (`AllowDirect`,
// `PostQuantumEncryption`) apply in every mode.
type PerformanceProfile struct {
	WindowType WindowType          `json:"window_type"`
	WindowSize *WindowSizeSettings `json:"window_size"`
	// setting this to true exposes the real source IP to the provider
	AllowDirect bool `json:"allow_direct"`
	// enable post-quantum e2e encryption to providers that support it.
	// Opportunistic: providers without support fall back to plaintext at
	// this layer.
	PostQuantumEncryption bool `json:"post_quantum_encryption"`
}

type WindowSizeSettings struct {
	WindowSizeMin int `json:"window_size_min"`
	// the minimumum number of items in the windows that must be connected via p2p only
	// leave 0 for default behavior
	WindowSizeMinP2pOnly int `json:"window_size_min_p2p_only"`
	// inclusive, soft limit
	WindowSizeMax int `json:"window_size_max"`
	// leave 0 to disable (no hard limit)
	WindowSizeHardMax int `json:"window_size_hard_max"`
	// clients per source
	// leave 0 to use the default value
	WindowSizeReconnectScale float64 `json:"window_size_reconnect_scale"`
	// leave 0 to disable
	KeepHealthiestCount int `json:"keep_healthiest_count"`
	// leave 0 to use the default value
	Ulimit int `json:"ulimit"`
}

/**
 * =============================================================
 * Utils for encoding/decoding base58, box encryption/decryption
 * Used for fetching the wallet address from Solflare
 * =============================================================
 */

func EncodeBase58(data []byte) string {
	return base58Encode(data)
}

func DecodeBase58(data string) ([]byte, error) {
	result := base58Decode(data)
	if len(result) == 0 {
		err := fmt.Errorf("DecodeBase58 error: invalid base58 string")
		glog.Errorf("DecodeBase58 error: %v", err)
		return nil, err
	}

	return result, nil

}

func EncryptData(data []byte, nonceBase58, sharedSecretBase58 string) (string, error) {
	nonce := base58Decode(nonceBase58)
	sharedSecret := base58Decode(sharedSecretBase58)

	if len(nonce) != 24 {
		return "", fmt.Errorf("invalid nonce length")
	}

	if len(sharedSecret) != 32 {
		return "", fmt.Errorf("invalid shared secret length")
	}

	var n [24]byte
	var k [32]byte
	copy(n[:], nonce)
	copy(k[:], sharedSecret)

	// Encrypt the data
	encrypted := box.SealAfterPrecomputation(nil, data, &n, &k)

	// Return base58 encoded encrypted data
	return base58Encode(encrypted), nil
}

func GenerateNonce() string {
	var nonce [24]byte

	// Use crypto/rand to fill the nonce with random bytes
	if _, err := rand.Read(nonce[:]); err != nil {
		// In a production system we would handle this error properly
		// but for a cryptographic random source, this should rarely if ever happen
		glog.Errorf("Failed to generate random nonce: %v", err)
		panic(err)
	}

	return base58Encode(nonce[:])
}

func DecryptData(encryptedDataBase58, nonceBase58, sharedSecretBase58 string) ([]byte, error) {
	encryptedData := base58Decode(encryptedDataBase58)
	nonce := base58Decode(nonceBase58)
	sharedSecret := base58Decode(sharedSecretBase58)

	if len(nonce) != 24 {
		return nil, fmt.Errorf("invalid nonce length")
	}

	if len(sharedSecret) != 32 {
		return nil, fmt.Errorf("invalid shared secret length")
	}

	var n [24]byte
	var k [32]byte
	copy(n[:], nonce)
	copy(k[:], sharedSecret)

	decrypted, ok := box.OpenAfterPrecomputation(nil, encryptedData, &n, &k)
	if !ok {
		return nil, fmt.Errorf("decryption failed")
	}

	return decrypted, nil
}

func GenerateSharedSecret(privateKey, publicKey []byte) ([]byte, error) {
	if len(privateKey) != 32 || len(publicKey) != 32 {
		return nil, fmt.Errorf("invalid key length")
	}

	var priv, pub [32]byte
	copy(priv[:], privateKey)
	copy(pub[:], publicKey)

	shared := new([32]byte)
	box.Precompute(shared, &pub, &priv)

	return shared[:], nil
}

// WalletKeyPair is an ephemeral Curve25519 keypair (base58-encoded) for the
// wallet-connect (Phantom/Solflare) NaCl-box handshake. Apple apps generate this
// with CryptoKit; desktop (cgo) apps that lack a Curve25519 primitive call
// GenerateWalletKeyPair so the keypair is wire-compatible with
// GenerateSharedSecret / EncryptData / DecryptData.
type WalletKeyPair struct {
	PrivateKeyBase58 string
	PublicKeyBase58  string
}

func GenerateWalletKeyPair() (*WalletKeyPair, error) {
	publicKey, privateKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &WalletKeyPair{
		PrivateKeyBase58: base58Encode(privateKey[:]),
		PublicKeyBase58:  base58Encode(publicKey[:]),
	}, nil
}
