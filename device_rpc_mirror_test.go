package sdk

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"reflect"
	"testing"

	"github.com/urnetwork/connect"
)

// The device rpc crosses several hand-mirrored value structs. A mirror that
// forgets a field drops it SILENTLY — the 2026-07 `NetworkPeer` drop broke
// the same-network allow-direct force on every hosted-device platform, and
// `CountryLocationId`, `Stable`, and `StrongPrivacy` had the same defect.
// Zero-valued fixtures let deep-equality round trips pass anyway, so these
// tests fill EVERY exported source field with a non-zero value and round
// trip through the mirror AND the gob wire: any unmapped field — including
// one added in the future — fails immediately.

// fillNonZero sets every settable field of v to a non-zero value,
// recursively. Unsupported kinds fail the test so new field types get a
// conscious mapping here.
func fillNonZero(t *testing.T, v reflect.Value, seed *int) {
	*seed += 1
	// types with unexported internals get well-formed values
	switch v.Type() {
	case reflect.TypeOf((*Id)(nil)):
		v.Set(reflect.ValueOf(NewId()))
		return
	case reflect.TypeOf(connect.Id{}):
		v.Set(reflect.ValueOf(connect.NewId()))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillNonZero(t, v.Elem(), seed)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i += 1 {
			f := v.Field(i)
			if !f.CanSet() {
				continue
			}
			fillNonZero(t, f, seed)
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(*seed))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(*seed))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(*seed))
	case reflect.String:
		v.SetString(fmt.Sprintf("v%d", *seed))
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(t, elem, seed)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillNonZero(t, k, seed)
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(t, e, seed)
		m.SetMapIndex(k, e)
		v.Set(m)
	default:
		t.Fatalf("fillNonZero: unsupported kind %s for %s — add a mapping", v.Kind(), v.Type())
	}
}

// gobRoundTrip runs a value through the rpc wire encoding, so a mirror field
// gob cannot carry (unexported, ignored) also fails the round trip.
func gobRoundTrip[T any](t *testing.T, value T) T {
	var buf bytes.Buffer
	connect.AssertEqual(t, gob.NewEncoder(&buf).Encode(value), nil)
	var out T
	connect.AssertEqual(t, gob.NewDecoder(&buf).Decode(&out), nil)
	return out
}

func TestRpcMirrorConnectLocationComplete(t *testing.T) {
	seed := 0
	location := &ConnectLocation{}
	fillNonZero(t, reflect.ValueOf(location), &seed)

	wired := gobRoundTrip(t, newDeviceRemoteConnectLocationValue(location))
	connect.AssertEqual(t, wired.toConnectLocation(), location)
}

func TestRpcMirrorConnectLocationIdComplete(t *testing.T) {
	seed := 0
	connectLocationId := &ConnectLocationId{}
	fillNonZero(t, reflect.ValueOf(connectLocationId), &seed)

	wired := gobRoundTrip(t, newDeviceRemoteConnectLocationId(connectLocationId))
	connect.AssertEqual(t, wired.toConnectLocationId(), connectLocationId)
}

func TestRpcMirrorProviderIdentityComplete(t *testing.T) {
	seed := 0
	providerIdentity := &ProviderIdentity{}
	fillNonZero(t, reflect.ValueOf(providerIdentity), &seed)

	wired := gobRoundTrip(t, newProviderIdentityRpc(providerIdentity))
	connect.AssertEqual(t, wired.toProviderIdentity(), providerIdentity)
}

func TestRpcMirrorTransferPathComplete(t *testing.T) {
	seed := 0
	transferPath := &TransferPath{}
	fillNonZero(t, reflect.ValueOf(transferPath), &seed)

	wired := gobRoundTrip(t, newDeviceRemoteTransferPath(transferPath))
	connect.AssertEqual(t, wired.toTransferPath(), transferPath)
}

// PerformanceProfile crosses whole-struct inside its rpc wrapper; the gob
// leg still guards against future gob-hostile fields.
func TestRpcGobPerformanceProfileComplete(t *testing.T) {
	seed := 0
	performanceProfile := &PerformanceProfile{}
	fillNonZero(t, reflect.ValueOf(performanceProfile), &seed)

	wired := gobRoundTrip(t, &DevicePerformanceProfile{PerformanceProfile: performanceProfile})
	connect.AssertEqual(t, wired.PerformanceProfile, performanceProfile)
}
