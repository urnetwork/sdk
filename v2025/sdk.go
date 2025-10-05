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
	"runtime/debug"
	// "strings"

	// "net/http"
	// _ "net/http/pprof"

	"github.com/golang/glog"

	"github.com/btcsuite/btcutil/base58"
	"github.com/urnetwork/connect/v2025"
	"github.com/urnetwork/connect/v2025/protocol"
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
	debug.SetGCPercent(10)

	initGlog()

	// initPprof()
}

func initGlog() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to stdout
	os.Stderr = os.Stdout
}

func SetMemoryLimit(limit int64) {
	connect.ResizeMessagePools(limit / 8)
	debug.SetMemoryLimit(limit)
}

func FreeMemory() {
	connect.ClearMessagePools()
	debug.FreeOSMemory()
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

// func initPprof() {
// 	go func() {
// 		glog.Infof("pprof = %s\n", http.ListenAndServe(":6060", nil))
// 	}()
// }

// this value is set via the linker, e.g.
// -ldflags "-X sdk.Version=$WARP_VERSION-$WARP_VERSION_CODE"
const Version string = "2025.10.4+748449170"

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
	if self.ConnectLocationId == nil {
		return b.ConnectLocationId == nil
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

/**
 * =============================================================
 * Utils for encoding/decoding base58, box encryption/decryption
 * Used for fetching the wallet address from Solflare
 * =============================================================
 */

func EncodeBase58(data []byte) string {
	return base58.Encode(data)
}

func DecodeBase58(data string) ([]byte, error) {
	result := base58.Decode(data)
	if len(result) == 0 {
		err := fmt.Errorf("DecodeBase58 error: invalid base58 string")
		glog.Errorf("DecodeBase58 error: %v", err)
		return nil, err
	}

	return result, nil

}

func EncryptData(data []byte, nonceBase58, sharedSecretBase58 string) (string, error) {
	nonce := base58.Decode(nonceBase58)
	sharedSecret := base58.Decode(sharedSecretBase58)

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
	return base58.Encode(encrypted), nil
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

	return base58.Encode(nonce[:])
}

func DecryptData(encryptedDataBase58, nonceBase58, sharedSecretBase58 string) ([]byte, error) {
	encryptedData := base58.Decode(encryptedDataBase58)
	nonce := base58.Decode(nonceBase58)
	sharedSecret := base58.Decode(sharedSecretBase58)

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
