//go:build !sdk_mobile_bind

package sdk

import (
	"context"
	"reflect"
	"testing"
)

// Native hosts retain the same named interface, exact method signatures and
// settings field type. A public alias to a newly private type would change
// reflection and native binding discovery even if Go assignments still worked.
func TestNativeLocalDeviceApiPreservesIdentityAndMethods(t *testing.T) {
	actual := reflect.TypeFor[LocalDeviceApi]()
	settings := reflect.TypeFor[DeviceLocalSettings]()
	if actual.Name() != "LocalDeviceApi" || actual.PkgPath() != settings.PkgPath() || actual.Kind() != reflect.Interface {
		t.Fatalf("native local API identity changed: %v", actual)
	}
	field, ok := settings.FieldByName("LocalApi")
	if !ok || field.Type != actual {
		t.Fatalf("native LocalApi field changed: %v", field.Type)
	}
	wanted := reflect.TypeFor[interface {
		Get(context.Context, string, string) ([]byte, error)
		Post(context.Context, string, []byte, string) ([]byte, error)
	}]()
	if actual.NumMethod() != wanted.NumMethod() {
		t.Fatalf("native local API methods=%d, want %d", actual.NumMethod(), wanted.NumMethod())
	}
	for index := range wanted.NumMethod() {
		method := wanted.Method(index)
		got, ok := actual.MethodByName(method.Name)
		if !ok || got.Type != method.Type {
			t.Errorf("native %s signature=%v, want %v", method.Name, got.Type, method.Type)
		}
	}
}
