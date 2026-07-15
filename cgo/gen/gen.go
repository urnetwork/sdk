// Generates the c abi for the full sdk surface.
//
// Usage: go run ./gen (from the cgo module root)
//
// Emits:
//
//	exports_gen.go, exports_gen_unix.go  - cgo export wrappers
//	callbacks.h, callbacks.c             - c callback typedefs and invoke shims
//	include/urnetwork_sdk.h              - the public c header
//	include/urnetwork_sdk.def            - windows module definition (for import libs)
//	coverage_report.txt                  - what is exported, what is skipped and why
//
// Mapping model (hybrid):
//   - behavioral objects cross as opaque uint64_t handles (release with urnet_release)
//   - data structs, slices, and maps cross as json strings
//   - sdk.Id crosses as a uuid string; sdk.Time as unix epoch milliseconds (0 = none)
//   - listener/callback interfaces cross as c function pointers + user_data
//   - []byte parameters cross as (const uint8_t*, int32_t)
package main

import (
	"fmt"
	"go/constant"
	"go/format"
	"go/types"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

const sdkPath = "github.com/urnetwork/sdk"

// behavioral types cross the abi as opaque handles
var behavioralTypes = map[string]bool{
	"NetworkSpaceManager":        true,
	"NetworkSpace":               true,
	"Api":                        true,
	"AsyncLocalState":            true,
	"LocalState":                 true,
	"Device":                     true,
	"DeviceLocal":                true,
	"DeviceRemote":               true,
	"DeviceStats":                true,
	"ProxyDevice":                true,
	"Sub":                        true,
	"IoLoop":                     true,
	"Tunnel":                     true,
	"ConnectGrid":                true,
	"DeviceLocalKeyMaterial":     true,
	"DeviceRpcKeyMaterial":       true,
	"WebsocketDeviceRpcDialer":   true,
	"WebsocketDeviceRpcListener": true,

	"AccountPreferencesViewController":    true,
	"AccountViewController":               true,
	"BlockActionViewController":           true,
	"ConnectViewController":               true,
	"ContractViewController":              true,
	"ContractDetailsViewController":       true,
	"DevicesViewController":               true,
	"FeedbackViewController":              true,
	"LocationsViewController":             true,
	"LoginViewController":                 true,
	"NetworkNameValidationViewController": true,
	"NetworkUserViewController":           true,
	"PeerViewController":                  true,
	"ProvideViewController":               true,
	"ReferralCodeViewController":          true,
	"WalletViewController":                true,
}

// skipped types are not exported. mirror the gomobile validate exclusions
// (see build/Makefile): rpc gob internals, testing and platform constructors.
var skipTypes = map[string]string{
	"DeviceLocalRpc":  "rpc gob internal (macOS parity: ignored)",
	"DeviceRemoteRpc": "rpc gob internal (macOS parity: ignored)",
}

var skipTypePatterns = []*regexp.Regexp{
	// gob rpc payload types, e.g. DeviceRemoteSyncRpc, BlockActionOverrideRpc
	regexp.MustCompile(`Rpc$`),
	regexp.MustCompile(`^Rpc`),
}

// explicitly not skipped even though they match skipTypePatterns
var keepTypes = map[string]bool{}

var skipFuncs = map[string]string{
	"NewPlatformNetworkSpace": "platform constructor (macOS parity: ignored)",
	"NewPlatformDeviceLocal":  "platform constructor (macOS parity: ignored)",
	"RequireIdFromBytes":      "panics on bad input; use urnet_parse_id",
	"MessagePoolGet":          "pool buffers must not cross the abi; use urnet_device_local_send_packet",
	"MessagePoolGetRaw":       "pool buffers must not cross the abi",
	"MessagePoolReturn":       "pool buffers must not cross the abi",
	"MessagePoolCheck":        "pool internal",
	"DecodeBase58":            "byte buffer result; manual export in exports_manual.go",
	"DecryptData":             "byte buffer result; manual export in exports_manual.go",
	"GenerateSharedSecret":    "byte buffer result; manual export in exports_manual.go",
}

var skipFuncPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^Testing_`),
	// list constructors: lists cross the abi as json arrays
	regexp.MustCompile(`^New[A-Za-z0-9]*List$`),
}

var skipMethods = map[string]string{
	"DeviceLocal.Ctx":                                "go context does not cross the abi",
	"DeviceLocal.SetUpgradeMuxSettings":              "connect internal type (macOS parity: ignored)",
	"DeviceLocal.SetClientSecurityPolicyGenerator":   "func param (macOS parity: ignored)",
	"DeviceLocal.SetProviderSecurityPolicyGenerator": "func param (macOS parity: ignored)",
	"DeviceLocal.AddReceivePacketCallback":           "func param (macOS parity: ignored); use urnet_device_local_add_receive_packet",
	"DeviceLocal.SendPacketNoCopy":                   "pool ownership does not cross the abi; use urnet_device_local_send_packet",
	"WebsocketDeviceRpcDialer.Dial":                  "net.Conn internal; used by DeviceRemote internally",
	"WebsocketDeviceRpcListener.Accept":              "net.Conn internal; used by DeviceLocal internally",

	// byte results cross via the buffer-out pattern, see exports_manual.go
	"DeviceLocal.GetClientKeySeed":                       "manual export urnet_device_local_get_client_key_seed",
	"DeviceLocal.GetProvideTlsCertificatePem":            "manual export urnet_device_local_get_provide_tls_certificate_pem",
	"DeviceLocal.GetProvideTlsPrivateKeyPem":             "manual export urnet_device_local_get_provide_tls_private_key_pem",
	"DeviceLocalKeyMaterial.GetClientKeySeed":            "manual export urnet_device_local_key_material_get_client_key_seed",
	"DeviceLocalKeyMaterial.GetProvideTlsCertificatePem": "manual export urnet_device_local_key_material_get_provide_tls_certificate_pem",
	"DeviceLocalKeyMaterial.GetProvideTlsPrivateKeyPem":  "manual export urnet_device_local_key_material_get_provide_tls_private_key_pem",
}

// symbols that only exist on unix-like targets (see device_local_ioloop.go)
var unixOnlySymbols = map[string]bool{
	"IoLoop":             true,
	"NewIoLoop":          true,
	"IoLoopDoneCallback": true,
}

// c names reserved by hand-written exports in the cgo package
var reservedCNames = map[string]bool{
	"urnet_version":           true,
	"urnet_free_string":       true,
	"urnet_release":           true,
	"urnet_live_handle_count": true,
}

func main() {
	g, err := load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
	if err := g.run(); err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}
}

type gen struct {
	pkg   *types.Package
	scope *types.Scope

	errorType  types.Type
	deviceType *types.Named

	// callback interfaces referenced by emitted functions
	callbacks map[string]*types.Named
	// data types referenced by emitted functions (for the header reference docs)
	dataTypes map[string]*types.Named
	// tags per data type struct
	// c function name -> description, for collision checks and the def file
	cNames map[string]string

	exports   []*export
	skipped   []skipRecord
	constants []constRecord
	usedUnix  bool
	// concrete behavioral types that implement the Device interface
	deviceDerived map[string]bool
}

type export struct {
	cName    string
	unixOnly bool
	goCode   string
	cDecl    string
	group    string
	sig      *exportSig
}

// exportSig is the structured form of an emitted function, for the hpp emitter
type exportSig struct {
	recvName string // behavioral type name, or "" for package functions
	goName   string
	params   []paramInfo
	result   *typeInfo
	hasError bool
}

type paramInfo struct {
	name string
	info typeInfo
	cb   *callbackInfo // set when info.kind == kindCallback
}

type skipRecord struct {
	symbol string
	reason string
}

type constRecord struct {
	goName  string
	cName   string
	literal string // c literal (also valid c++)
}

func load() (*gen, error) {
	config := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedImports | packages.NeedDeps,
	}
	pkgs, err := packages.Load(config, sdkPath)
	if err != nil {
		return nil, err
	}
	if packages.PrintErrors(pkgs) > 0 {
		return nil, fmt.Errorf("package load errors")
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("expected one package, got %d", len(pkgs))
	}
	g := &gen{
		pkg:           pkgs[0].Types,
		scope:         pkgs[0].Types.Scope(),
		errorType:     types.Universe.Lookup("error").Type(),
		callbacks:     map[string]*types.Named{},
		dataTypes:     map[string]*types.Named{},
		cNames:        map[string]string{},
		deviceDerived: map[string]bool{},
	}
	if obj := g.scope.Lookup("Device"); obj != nil {
		if named, ok := types.Unalias(obj.Type()).(*types.Named); ok {
			g.deviceType = named
		}
	}
	return g, nil
}

func (g *gen) run() error {
	names := g.scope.Names()
	sort.Strings(names)

	for _, name := range names {
		obj := g.scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch obj := obj.(type) {
		case *types.Func:
			g.emitFunc(obj)
		case *types.TypeName:
			g.emitType(obj)
		case *types.Const:
			g.emitConst(obj)
		}
	}

	if err := g.write(); err != nil {
		return err
	}

	unmappable := 0
	for _, s := range g.skipped {
		if strings.HasPrefix(s.reason, "unmappable") {
			unmappable += 1
		}
	}
	fmt.Printf("✅ generated %d c functions, %d callback types, %d constants; skipped %d (%d unmappable) — see coverage_report.txt\n",
		len(g.exports), len(g.callbacks), len(g.constants), len(g.skipped), unmappable)
	return nil
}

// ---------------------------------------------------------------------------
// classification

type kind int

const (
	kindVoid kind = iota
	kindBool
	kindInt
	kindFloat64
	kindFloat32
	kindString
	kindId
	kindTime
	kindHandle
	kindJson
	kindBytes
	kindCallback
	kindError
	kindBad
)

type typeInfo struct {
	kind    kind
	t       types.Type
	named   *types.Named
	pointer bool
	reason  string
}

func bad(reason string) typeInfo {
	return typeInfo{kind: kindBad, reason: reason}
}

func (g *gen) classify(t types.Type) typeInfo {
	t = types.Unalias(t)

	if types.Identical(t, g.errorType) {
		return typeInfo{kind: kindError, t: t}
	}

	switch t := t.(type) {
	case *types.Basic:
		return g.classifyBasic(t, t)
	case *types.Pointer:
		elem := types.Unalias(t.Elem())
		named, ok := elem.(*types.Named)
		if !ok {
			return bad(fmt.Sprintf("pointer to %s", elem.String()))
		}
		return g.classifyNamed(named, t, true)
	case *types.Named:
		return g.classifyNamed(t, t, false)
	case *types.Slice:
		elem := types.Unalias(t.Elem())
		if basic, ok := elem.(*types.Basic); ok && basic.Kind() == types.Uint8 {
			return typeInfo{kind: kindBytes, t: t}
		}
		if info := g.jsonable(elem); info.kind == kindBad {
			return bad(fmt.Sprintf("slice of %s: %s", elem.String(), info.reason))
		}
		return typeInfo{kind: kindJson, t: t}
	case *types.Map:
		return typeInfo{kind: kindJson, t: t}
	case *types.Signature:
		return bad("func type")
	case *types.Interface:
		return bad("anonymous interface")
	}
	return bad(t.String())
}

func (g *gen) classifyBasic(basic *types.Basic, original types.Type) typeInfo {
	switch basic.Kind() {
	case types.Bool:
		return typeInfo{kind: kindBool, t: original}
	case types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint16, types.Uint32, types.Uint64:
		return typeInfo{kind: kindInt, t: original}
	case types.Float64:
		return typeInfo{kind: kindFloat64, t: original}
	case types.Float32:
		return typeInfo{kind: kindFloat32, t: original}
	case types.String:
		return typeInfo{kind: kindString, t: original}
	}
	return bad(fmt.Sprintf("basic %s", basic.String()))
}

func (g *gen) classifyNamed(named *types.Named, original types.Type, pointer bool) typeInfo {
	name := named.Obj().Name()
	if named.Obj().Pkg() == nil {
		return bad(fmt.Sprintf("universe type %s", name))
	}
	if named.Obj().Pkg().Path() != sdkPath {
		return bad(fmt.Sprintf("external type %s", types.TypeString(named, nil)))
	}
	if !named.Obj().Exported() {
		return bad(fmt.Sprintf("unexported type %s", name))
	}

	if name == "Id" && pointer {
		return typeInfo{kind: kindId, t: original, named: named, pointer: pointer}
	}
	if name == "Time" && pointer {
		return typeInfo{kind: kindTime, t: original, named: named, pointer: pointer}
	}
	if reason, ok := g.typeSkipReason(name); ok {
		return bad(fmt.Sprintf("skipped type %s (%s)", name, reason))
	}
	if behavioralTypes[name] {
		return typeInfo{kind: kindHandle, t: original, named: named, pointer: pointer}
	}
	if _, ok := named.Underlying().(*types.Interface); ok {
		// non-behavioral sdk interface: implemented by the app, crosses as c callbacks
		return typeInfo{kind: kindCallback, t: original, named: named, pointer: pointer}
	}
	if basic, ok := named.Underlying().(*types.Basic); ok {
		return g.classifyBasic(basic, original)
	}
	if _, ok := named.Underlying().(*types.Struct); ok {
		return typeInfo{kind: kindJson, t: original, named: named, pointer: pointer}
	}
	if info := g.classify(named.Underlying()); info.kind != kindBad {
		return info
	}
	return bad(fmt.Sprintf("type %s (%s)", name, named.Underlying().String()))
}

// jsonable reports whether a type can round trip as json
func (g *gen) jsonable(t types.Type) typeInfo {
	info := g.classify(t)
	switch info.kind {
	case kindHandle, kindCallback, kindBytes, kindError, kindBad:
		if info.kind != kindBad {
			return bad(info.t.String())
		}
		return info
	}
	return info
}

func (g *gen) typeSkipReason(name string) (string, bool) {
	if reason, ok := skipTypes[name]; ok {
		return reason, true
	}
	if keepTypes[name] {
		return "", false
	}
	for _, p := range skipTypePatterns {
		if p.MatchString(name) {
			return "rpc gob internal (macOS parity: ignored)", true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// emission

func (g *gen) emitConst(obj *types.Const) {
	if obj.Name() == "Version" {
		// crosses via urnet_version(), which returns the linker-set value
		g.skip("Version", "crosses via urnet_version()")
		return
	}
	if _, ok := obj.Type().(*types.Basic); !ok {
		// typed constants (aliases to basic) are fine; others are not expected
		if _, ok := types.Unalias(obj.Type()).(*types.Basic); !ok {
			basicUnderlying := false
			if named, ok := types.Unalias(obj.Type()).(*types.Named); ok {
				_, basicUnderlying = named.Underlying().(*types.Basic)
			}
			if !basicUnderlying {
				g.skip(obj.Name(), "unmappable: non-basic constant")
				return
			}
		}
	}
	name := "URNET_" + strings.ToUpper(snake(obj.Name()))
	val := obj.Val()
	switch val.Kind() {
	case constant.Int:
		g.constants = append(g.constants, constRecord{obj.Name(), name, val.ExactString()})
	case constant.String:
		g.constants = append(g.constants, constRecord{obj.Name(), name, val.ExactString()})
	case constant.Float:
		f, _ := constant.Float64Val(val)
		g.constants = append(g.constants, constRecord{obj.Name(), name, fmt.Sprintf("%g", f)})
	default:
		g.skip(obj.Name(), "unmappable: constant kind")
	}
}

func (g *gen) emitFunc(obj *types.Func) {
	name := obj.Name()
	if reason, ok := skipFuncs[name]; ok {
		g.skip(name, "skipped: "+reason)
		return
	}
	for _, p := range skipFuncPatterns {
		if p.MatchString(name) {
			g.skip(name, "skipped: pattern "+p.String())
			return
		}
	}
	cName := "urnet_" + snake(name)
	g.emitCallable(cName, name, nil, "", obj, "functions")
}

func (g *gen) emitType(obj *types.TypeName) {
	name := obj.Name()
	if reason, ok := g.typeSkipReason(name); ok {
		g.skip(name, "skipped: "+reason)
		return
	}
	named, ok := types.Unalias(obj.Type()).(*types.Named)
	if !ok {
		// plain aliases (ByteCount, LocationType, ...) need no exports
		g.skip(name, "alias: crosses as its underlying c type")
		return
	}
	if !behavioralTypes[name] {
		// data and callback types generate no direct exports;
		// they are emitted on demand from function signatures
		info := g.classifyNamed(named, named, true)
		switch info.kind {
		case kindJson:
			g.skip(name, "data: crosses as json")
		case kindCallback:
			g.skip(name, "callback interface: crosses as c function pointer")
		case kindId:
			g.skip(name, "crosses as uuid string")
		case kindTime:
			g.skip(name, "crosses as unix epoch milliseconds")
		default:
			g.skip(name, "no exports")
		}
		return
	}

	// behavioral: export the exported method set of *T (or the interface method set)
	var methodSet *types.MethodSet
	var recvInfo typeInfo
	if _, isInterface := named.Underlying().(*types.Interface); isInterface {
		methodSet = types.NewMethodSet(named)
		recvInfo = typeInfo{kind: kindHandle, t: named, named: named}
	} else {
		ptr := types.NewPointer(named)
		methodSet = types.NewMethodSet(ptr)
		recvInfo = typeInfo{kind: kindHandle, t: ptr, named: named, pointer: true}
	}

	// methods identical to the Device interface are only emitted once, on Device
	var deviceIface *types.Interface
	if g.deviceType != nil && name != "Device" {
		if iface, ok := g.deviceType.Underlying().(*types.Interface); ok {
			var target types.Type = named
			if _, isIface := named.Underlying().(*types.Interface); !isIface {
				target = types.NewPointer(named)
			}
			if types.Implements(target, iface) {
				deviceIface = iface
				g.deviceDerived[name] = true
			}
		}
	}

	for i := 0; i < methodSet.Len(); i += 1 {
		sel := methodSet.At(i)
		m := sel.Obj().(*types.Func)
		if !m.Exported() {
			continue
		}
		qualified := name + "." + m.Name()
		if reason, ok := skipMethods[qualified]; ok {
			g.skip(qualified, "skipped: "+reason)
			continue
		}
		if deviceIface != nil {
			if dm := lookupIfaceMethod(deviceIface, m.Name()); dm != nil {
				if types.Identical(dm.Type(), sel.Type()) {
					g.skip(qualified, "device interface method: use urnet_device_"+snake(m.Name()))
					continue
				}
			}
		}
		cName := "urnet_" + snake(name) + "_" + snake(m.Name())
		g.emitCallable(cName, qualified, &recvInfo, name, m, name)
	}
}

func lookupIfaceMethod(iface *types.Interface, name string) *types.Func {
	for i := 0; i < iface.NumMethods(); i += 1 {
		if iface.Method(i).Name() == name {
			return iface.Method(i)
		}
	}
	return nil
}

// emitCallable generates one c function wrapping a package func or method
func (g *gen) emitCallable(cName string, symbol string, recv *typeInfo, recvName string, fn *types.Func, group string) {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		g.skip(symbol, "unmappable: not a signature")
		return
	}
	if recv == nil {
		// for methods the receiver comes from the method set (sig.Recv is set); for
		// package funcs there is none
		sig = fn.Type().(*types.Signature)
	}
	if sig.Variadic() {
		g.skip(symbol, "unmappable: variadic")
		return
	}
	if sig.TypeParams() != nil && sig.TypeParams().Len() > 0 {
		g.skip(symbol, "unmappable: generic")
		return
	}

	unixOnly := unixOnlySymbols[strings.Split(symbol, ".")[0]] || unixOnlySymbols[recvName]

	var goParams []string // go wrapper parameter declarations
	var cParams []string  // c declaration parameters
	var convert []string  // conversion statements
	var callArgs []string // arguments to the sdk call
	var sigParams []paramInfo

	fail := func(reason string) {
		g.skip(symbol, "unmappable: "+reason)
	}

	// results first, to know the zero return for early exits
	results := sig.Results()
	var resultInfo *typeInfo
	hasError := false
	if results.Len() > 0 {
		last := g.classify(results.At(results.Len() - 1).Type())
		if last.kind == kindError {
			hasError = true
		}
		nonError := results.Len()
		if hasError {
			nonError -= 1
		}
		if nonError > 1 {
			fail("multiple results")
			return
		}
		for i := 0; i < nonError; i += 1 {
			info := g.classify(results.At(i).Type())
			if info.kind == kindBad {
				fail("result: " + info.reason)
				return
			}
			if info.kind == kindError || info.kind == kindCallback || info.kind == kindBytes {
				fail("result: " + info.t.String())
				return
			}
			resultInfo = &info
		}
	}

	cRet, goRet, zeroRet := g.returnForms(resultInfo, hasError)

	if recv != nil {
		goParams = append(goParams, "self C.uint64_t")
		cParams = append(cParams, "uint64_t self")
		convert = append(convert,
			fmt.Sprintf("\tself_, ok := resolveHandle[%s](uint64(self), %q)", g.goType(recv.t), cName),
			"\tif !ok {",
			"\t\treturn"+zeroRet,
			"\t}",
		)
	}

	params := sig.Params()
	for i := 0; i < params.Len(); i += 1 {
		p := params.At(i)
		pName := paramName(p, i)
		info := g.classify(p.Type())
		var cbForParam *callbackInfo
		switch info.kind {
		case kindBad:
			fail(fmt.Sprintf("param %s: %s", pName, info.reason))
			return
		case kindError:
			fail(fmt.Sprintf("param %s: error", pName))
			return
		case kindBool:
			goParams = append(goParams, pName+" C.bool")
			cParams = append(cParams, "bool "+snake(pName))
			callArgs = append(callArgs, g.convertPrimitive(p.Type(), "bool("+pName+")"))
		case kindInt:
			goParams = append(goParams, pName+" C.int64_t")
			cParams = append(cParams, "int64_t "+snake(pName))
			callArgs = append(callArgs, g.convertPrimitive(p.Type(), "int64("+pName+")"))
		case kindFloat64:
			goParams = append(goParams, pName+" C.double")
			cParams = append(cParams, "double "+snake(pName))
			callArgs = append(callArgs, g.convertPrimitive(p.Type(), "float64("+pName+")"))
		case kindFloat32:
			goParams = append(goParams, pName+" C.float")
			cParams = append(cParams, "float "+snake(pName))
			callArgs = append(callArgs, g.convertPrimitive(p.Type(), "float32("+pName+")"))
		case kindString:
			goParams = append(goParams, pName+" *C.char")
			cParams = append(cParams, "const char* "+snake(pName))
			callArgs = append(callArgs, g.convertPrimitive(p.Type(), "goString("+pName+")"))
		case kindId:
			goParams = append(goParams, pName+" *C.char")
			cParams = append(cParams, "const char* "+snake(pName))
			callArgs = append(callArgs, fmt.Sprintf("goId(%s, %q)", pName, cName))
		case kindTime:
			goParams = append(goParams, pName+" C.int64_t")
			cParams = append(cParams, "int64_t "+snake(pName))
			callArgs = append(callArgs, "goTime("+pName+")")
		case kindHandle:
			goParams = append(goParams, pName+" C.uint64_t")
			cParams = append(cParams, "uint64_t "+snake(pName))
			convert = append(convert,
				fmt.Sprintf("\t%s_, ok := resolveHandle[%s](uint64(%s), %q)", pName, g.goType(info.t), pName, cName),
				"\tif !ok {",
				"\t\treturn"+zeroRet,
				"\t}",
			)
			callArgs = append(callArgs, pName+"_")
		case kindJson:
			goParams = append(goParams, pName+" *C.char")
			cParams = append(cParams, "const char* "+snake(pName)+"_json")
			if info.pointer {
				convert = append(convert,
					fmt.Sprintf("\tvar %s_ %s", pName, g.goType(info.t)),
					fmt.Sprintf("\tif %s != nil {", pName),
					fmt.Sprintf("\t\t%s_ = &%s{}", pName, g.goType(info.named)),
					fmt.Sprintf("\t\tif !goJson(%s, %s_, %q) {", pName, pName, cName),
					"\t\t\treturn"+zeroRet,
					"\t\t}",
					"\t}",
				)
			} else {
				convert = append(convert,
					fmt.Sprintf("\tvar %s_ %s", pName, g.goType(info.t)),
					fmt.Sprintf("\tif !goJson(%s, &%s_, %q) {", pName, pName, cName),
					"\t\treturn"+zeroRet,
					"\t}",
				)
			}
			callArgs = append(callArgs, pName+"_")
			g.recordDataType(info.t)
		case kindBytes:
			goParams = append(goParams, pName+" *C.uint8_t", pName+"_len C.int32_t")
			cParams = append(cParams, "const uint8_t* "+snake(pName), "int32_t "+snake(pName)+"_len")
			callArgs = append(callArgs, fmt.Sprintf("goBytes(%s, %s_len)", pName, pName))
		case kindCallback:
			cbInfo, err := g.callback(info.named)
			if err != nil {
				fail(fmt.Sprintf("param %s: %v", pName, err))
				return
			}
			cbForParam = cbInfo
			var fields []string
			for _, m := range cbInfo.methods {
				goParams = append(goParams, fmt.Sprintf("%s_%s C.%s", pName, snake(m.name), m.typedefName))
				cParams = append(cParams, fmt.Sprintf("%s %s_%s", m.typedefName, snake(pName), snake(m.name)))
				fields = append(fields, fmt.Sprintf("%s: %s_%s", m.fieldName, pName, snake(m.name)))
			}
			goParams = append(goParams, pName+"_user_data unsafe.Pointer")
			cParams = append(cParams, "void* "+snake(pName)+"_user_data")
			fields = append(fields, "userData: "+pName+"_user_data")
			primary := cbInfo.methods[0]
			convert = append(convert,
				fmt.Sprintf("\tvar %s_ %s", pName, g.goType(info.named)),
				fmt.Sprintf("\tif %s_%s != nil {", pName, snake(primary.name)),
				fmt.Sprintf("\t\t%s_ = &%s{%s}", pName, cbInfo.adapterName, strings.Join(fields, ", ")),
				"\t}",
			)
			callArgs = append(callArgs, pName+"_")
		default:
			fail(fmt.Sprintf("param %s: unhandled kind", pName))
			return
		}
		sigParams = append(sigParams, paramInfo{name: pName, info: info, cb: cbForParam})
	}

	if hasError {
		goParams = append(goParams, "outError **C.char")
		cParams = append(cParams, "char** out_error")
	}

	// build the call
	var call string
	if recv != nil {
		call = fmt.Sprintf("self_.%s(%s)", fn.Name(), strings.Join(callArgs, ", "))
	} else {
		call = fmt.Sprintf("sdk.%s(%s)", fn.Name(), strings.Join(callArgs, ", "))
	}

	var body []string
	body = append(body, fmt.Sprintf("\tdefer cgoGuard(%q)", cName))
	body = append(body, convert...)

	switch {
	case resultInfo == nil && !hasError:
		body = append(body, "\t"+call)
	case resultInfo == nil && hasError:
		body = append(body,
			"\terr := "+call,
			"\tif err != nil {",
			"\t\tsetErrorOut(outError, err)",
			"\t\treturn C.bool(false)",
			"\t}",
			"\treturn C.bool(true)",
		)
	default:
		if hasError {
			body = append(body,
				"\tr0, err := "+call,
				"\tif err != nil {",
				"\t\tsetErrorOut(outError, err)",
				"\t\treturn"+zeroRet,
				"\t}",
			)
		} else {
			body = append(body, "\tr0 := "+call)
		}
		body = append(body, g.resultReturn(*resultInfo, cName)...)
	}

	goCode := fmt.Sprintf("//export %s\nfunc %s(%s)%s {\n%s\n}\n",
		cName, cName, strings.Join(goParams, ", "), goRet, strings.Join(body, "\n"))
	cDecl := fmt.Sprintf("%s %s(%s);", cRet, cName, strings.Join(cParams, ", "))
	if len(cParams) == 0 {
		cDecl = fmt.Sprintf("%s %s(void);", cRet, cName)
	}

	if prev, exists := g.cNames[cName]; exists {
		fmt.Fprintf(os.Stderr, "c name collision: %s (%s and %s)\n", cName, prev, symbol)
		os.Exit(1)
	}
	if reservedCNames[cName] {
		fmt.Fprintf(os.Stderr, "c name collision with core export: %s\n", cName)
		os.Exit(1)
	}
	g.cNames[cName] = symbol
	if unixOnly {
		g.usedUnix = true
	}
	g.exports = append(g.exports, &export{
		cName:    cName,
		unixOnly: unixOnly,
		goCode:   goCode,
		cDecl:    cDecl,
		group:    group,
		sig: &exportSig{
			recvName: recvName,
			goName:   fn.Name(),
			params:   sigParams,
			result:   resultInfo,
			hasError: hasError,
		},
	})
}

// returnForms computes the c return type, go return type, and the go zero return
func (g *gen) returnForms(resultInfo *typeInfo, hasError bool) (cRet string, goRet string, zeroRet string) {
	if resultInfo == nil {
		if hasError {
			return "bool", " C.bool", " C.bool(false)"
		}
		return "void", "", ""
	}
	switch resultInfo.kind {
	case kindBool:
		return "bool", " C.bool", " C.bool(false)"
	case kindInt:
		return "int64_t", " C.int64_t", " 0"
	case kindFloat64:
		return "double", " C.double", " 0"
	case kindFloat32:
		return "float", " C.float", " 0"
	case kindString, kindId, kindJson:
		return "char*", " *C.char", " nil"
	case kindTime:
		return "int64_t", " C.int64_t", " 0"
	case kindHandle:
		return "uint64_t", " C.uint64_t", " 0"
	}
	return "void", "", ""
}

func (g *gen) resultReturn(info typeInfo, cName string) []string {
	switch info.kind {
	case kindBool:
		return []string{"\treturn C.bool(r0)"}
	case kindInt:
		return []string{"\treturn C.int64_t(r0)"}
	case kindFloat64:
		return []string{"\treturn C.double(r0)"}
	case kindFloat32:
		return []string{"\treturn C.float(r0)"}
	case kindString:
		return []string{"\treturn cString(string(r0))"}
	case kindId:
		return []string{"\treturn cId(r0)"}
	case kindTime:
		return []string{"\treturn cTime(r0)"}
	case kindHandle:
		if info.pointer {
			return []string{
				"\tif r0 == nil {",
				"\t\treturn 0",
				"\t}",
				"\treturn C.uint64_t(newHandle(r0))",
			}
		}
		return []string{"\treturn C.uint64_t(newHandle(r0))"}
	case kindJson:
		g.recordDataType(info.t)
		if info.pointer {
			return []string{
				"\tif r0 == nil {",
				"\t\treturn nil",
				"\t}",
				fmt.Sprintf("\treturn cJson(r0, %q)", cName),
			}
		}
		return []string{fmt.Sprintf("\treturn cJson(r0, %q)", cName)}
	}
	return []string{"\treturn"}
}

func (g *gen) convertPrimitive(t types.Type, inner string) string {
	// named types with basic underlying need an explicit conversion
	unaliased := types.Unalias(t)
	if named, ok := unaliased.(*types.Named); ok {
		return fmt.Sprintf("%s(%s)", g.goType(named), inner)
	}
	if basic, ok := unaliased.(*types.Basic); ok {
		switch basic.Kind() {
		case types.Int64, types.Bool, types.Float64, types.Float32, types.String:
			return inner
		default:
			return fmt.Sprintf("%s(%s)", basic.String(), inner)
		}
	}
	return inner
}

func (g *gen) goType(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string {
		if p.Path() == sdkPath {
			return "sdk"
		}
		return p.Name()
	})
}

// ---------------------------------------------------------------------------
// callback interfaces

type callbackMethod struct {
	name        string
	fieldName   string
	typedefName string
	// c typedef and shim source
	typedef  string
	shimDecl string
	shimDef  string
	// go adapter method source
	adapter string
	// structured form for the hpp emitter
	params  []paramInfo
	result  *typeInfo
	cParams []string
}

type callbackInfo struct {
	ifaceName   string
	adapterName string
	unixOnly    bool
	methods     []*callbackMethod
}

var callbackCache = map[string]*callbackInfo{}

func (g *gen) callback(named *types.Named) (*callbackInfo, error) {
	name := named.Obj().Name()
	if info, ok := callbackCache[name]; ok {
		return info, nil
	}
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("%s is not an interface", name)
	}

	base := strings.TrimSuffix(strings.TrimSuffix(name, "Listener"), "Callback")
	if base == "" {
		base = name
	}

	info := &callbackInfo{
		ifaceName:   name,
		adapterName: "cAdapter" + name,
		unixOnly:    unixOnlySymbols[name],
	}

	for i := 0; i < iface.NumMethods(); i += 1 {
		m := iface.Method(i)
		if !m.Exported() {
			return nil, fmt.Errorf("%s has unexported method %s", name, m.Name())
		}
		sig := m.Type().(*types.Signature)
		if sig.Variadic() {
			return nil, fmt.Errorf("%s.%s is variadic", name, m.Name())
		}

		typedefName := "urnet_" + snake(base) + "_cb"
		if iface.NumMethods() > 1 {
			typedefName = "urnet_" + snake(base) + "_" + snake(m.Name()) + "_cb"
		}

		cm := &callbackMethod{
			name:        m.Name(),
			fieldName:   "cb" + m.Name(),
			typedefName: typedefName,
		}

		var cParams []string       // typedef params after user_data
		var goShimArgs []string    // args passed to the shim from go
		var adapterParams []string // go adapter method params
		var pre []string           // adapter conversion statements
		var post []string          // adapter cleanup statements

		params := sig.Params()
		for j := 0; j < params.Len(); j += 1 {
			p := params.At(j)
			pName := paramName(p, j)
			pInfo := g.classify(p.Type())
			adapterParams = append(adapterParams, pName+" "+g.goType(p.Type()))
			cm.params = append(cm.params, paramInfo{name: pName, info: pInfo})
			switch pInfo.kind {
			case kindBool:
				cParams = append(cParams, "bool "+snake(pName))
				goShimArgs = append(goShimArgs, "C.bool(bool("+pName+"))")
			case kindInt:
				cParams = append(cParams, "int64_t "+snake(pName))
				goShimArgs = append(goShimArgs, "C.int64_t(int64("+pName+"))")
			case kindFloat64:
				cParams = append(cParams, "double "+snake(pName))
				goShimArgs = append(goShimArgs, "C.double(float64("+pName+"))")
			case kindFloat32:
				cParams = append(cParams, "float "+snake(pName))
				goShimArgs = append(goShimArgs, "C.float(float32("+pName+"))")
			case kindString:
				cParams = append(cParams, "const char* "+snake(pName))
				pre = append(pre, fmt.Sprintf("\t%s_ := cString(string(%s))", pName, pName))
				post = append(post, fmt.Sprintf("\tcStringFree(%s_)", pName))
				goShimArgs = append(goShimArgs, pName+"_")
			case kindId:
				cParams = append(cParams, "const char* "+snake(pName))
				pre = append(pre, fmt.Sprintf("\t%s_ := cId(%s)", pName, pName))
				post = append(post,
					fmt.Sprintf("\tif %s_ != nil {", pName),
					fmt.Sprintf("\t\tcStringFree(%s_)", pName),
					"\t}",
				)
				goShimArgs = append(goShimArgs, pName+"_")
			case kindTime:
				cParams = append(cParams, "int64_t "+snake(pName))
				goShimArgs = append(goShimArgs, "cTime("+pName+")")
			case kindHandle:
				// the c side owns the handle passed to the callback and must release it
				cParams = append(cParams, "uint64_t "+snake(pName))
				pre = append(pre, fmt.Sprintf("\t%s_ := C.uint64_t(newHandle(%s))", pName, pName))
				goShimArgs = append(goShimArgs, pName+"_")
			case kindJson:
				g.recordDataType(pInfo.t)
				cParams = append(cParams, "const char* "+snake(pName)+"_json")
				pre = append(pre, fmt.Sprintf("\t%s_ := cJson(%s, %q)", pName, pName, typedefName))
				post = append(post,
					fmt.Sprintf("\tif %s_ != nil {", pName),
					fmt.Sprintf("\t\tcStringFree(%s_)", pName),
					"\t}",
				)
				goShimArgs = append(goShimArgs, pName+"_")
			case kindBytes:
				cParams = append(cParams, "const uint8_t* "+snake(pName), "int32_t "+snake(pName)+"_len")
				pre = append(pre,
					fmt.Sprintf("\tvar %s_ *C.uint8_t", pName),
					fmt.Sprintf("\tif 0 < len(%s) {", pName),
					fmt.Sprintf("\t\t%s_ = (*C.uint8_t)(unsafe.Pointer(&%s[0]))", pName, pName),
					"\t}",
				)
				goShimArgs = append(goShimArgs, pName+"_", fmt.Sprintf("C.int32_t(len(%s))", pName))
			case kindError:
				cParams = append(cParams, "const char* "+snake(pName))
				pre = append(pre,
					fmt.Sprintf("\tvar %s_ *C.char", pName),
					fmt.Sprintf("\tif %s != nil {", pName),
					fmt.Sprintf("\t\t%s_ = cString(%s.Error())", pName, pName),
					"\t}",
				)
				post = append(post,
					fmt.Sprintf("\tif %s_ != nil {", pName),
					fmt.Sprintf("\t\tcStringFree(%s_)", pName),
					"\t}",
				)
				goShimArgs = append(goShimArgs, pName+"_")
			default:
				return nil, fmt.Errorf("%s.%s param %s: %s", name, m.Name(), pName, pInfo.reason)
			}
		}

		// callback return value
		results := sig.Results()
		cRet := "void"
		adapterRet := ""
		invoke := fmt.Sprintf("C.urnet_invoke_%s(self.%s, self.userData%s)",
			strings.TrimSuffix(strings.TrimPrefix(typedefName, "urnet_"), "_cb"),
			cm.fieldName,
			prefixJoin(goShimArgs))
		var ret []string
		if results.Len() > 1 {
			return nil, fmt.Errorf("%s.%s: multiple results", name, m.Name())
		}
		if results.Len() == 1 {
			rInfo := g.classify(results.At(0).Type())
			cm.result = &rInfo
			switch rInfo.kind {
			case kindBool:
				cRet = "bool"
				adapterRet = " " + g.goType(results.At(0).Type())
				pre = append(pre, "\tr0_ := "+invoke)
				ret = append(ret, fmt.Sprintf("\treturn %s", g.convertPrimitive(results.At(0).Type(), "bool(r0_)")))
			case kindInt:
				cRet = "int64_t"
				adapterRet = " " + g.goType(results.At(0).Type())
				pre = append(pre, "\tr0_ := "+invoke)
				ret = append(ret, fmt.Sprintf("\treturn %s", g.convertPrimitive(results.At(0).Type(), "int64(r0_)")))
			case kindString:
				cRet = "char*"
				adapterRet = " " + g.goType(results.At(0).Type())
				pre = append(pre, "\tr0_ := "+invoke)
				ret = append(ret,
					"\tr0 := goString(r0_)",
					"\tif r0_ != nil {",
					"\t\turnet_free_string(r0_)",
					"\t}",
					fmt.Sprintf("\treturn %s", g.convertPrimitive(results.At(0).Type(), "r0")),
				)
			default:
				return nil, fmt.Errorf("%s.%s: unsupported result %s", name, m.Name(), results.At(0).Type().String())
			}
		} else {
			pre = append(pre, "\t"+invoke)
		}

		shimName := strings.TrimSuffix(strings.TrimPrefix(typedefName, "urnet_"), "_cb")
		cParamsFull := append([]string{typedefName + " cb", "void* user_data"}, cParams...)
		cArgs := []string{"user_data"}
		for _, p := range cParams {
			parts := strings.Fields(p)
			cArgs = append(cArgs, parts[len(parts)-1])
		}

		cm.typedef = fmt.Sprintf("typedef %s (*%s)(void* user_data%s);", cRet, typedefName, prefixJoin(cParams))
		cm.shimDecl = fmt.Sprintf("%s urnet_invoke_%s(%s);", cRet, shimName, strings.Join(cParamsFull, ", "))
		returnKeyword := "return "
		if cRet == "void" {
			returnKeyword = ""
		}
		cm.shimDef = fmt.Sprintf("%s urnet_invoke_%s(%s) {\n\t%scb(%s);\n}",
			cRet, shimName, strings.Join(cParamsFull, ", "), returnKeyword, strings.Join(cArgs, ", "))

		var adapterBody []string
		adapterBody = append(adapterBody, fmt.Sprintf("\tdefer cgoGuard(%q)", typedefName))
		adapterBody = append(adapterBody, pre...)
		adapterBody = append(adapterBody, post...)
		adapterBody = append(adapterBody, ret...)
		cm.adapter = fmt.Sprintf("func (self *%s) %s(%s)%s {\n%s\n}\n",
			info.adapterName, m.Name(), strings.Join(adapterParams, ", "), adapterRet, strings.Join(adapterBody, "\n"))

		cm.cParams = cParams
		info.methods = append(info.methods, cm)
	}

	callbackCache[name] = info
	g.callbacks[name] = named
	return info, nil
}

func prefixJoin(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return ", " + strings.Join(items, ", ")
}

// ---------------------------------------------------------------------------
// data type reference docs

var recordedData = map[string]bool{}

func (g *gen) recordDataType(t types.Type) {
	t = types.Unalias(t)
	switch t := t.(type) {
	case *types.Pointer:
		g.recordDataType(t.Elem())
	case *types.Slice:
		g.recordDataType(t.Elem())
	case *types.Map:
		g.recordDataType(t.Elem())
	case *types.Named:
		if t.Obj().Pkg() == nil || t.Obj().Pkg().Path() != sdkPath {
			return
		}
		name := t.Obj().Name()
		if recordedData[name] || behavioralTypes[name] {
			return
		}
		if _, ok := t.Underlying().(*types.Struct); !ok {
			return
		}
		recordedData[name] = true
		g.dataTypes[name] = t
		// record nested types
		st := t.Underlying().(*types.Struct)
		for i := 0; i < st.NumFields(); i += 1 {
			f := st.Field(i)
			if f.Embedded() {
				// list wrappers embed exportedList[T]; record the element type
				if n, ok := types.Unalias(f.Type()).(*types.Named); ok && n.TypeArgs() != nil {
					for j := 0; j < n.TypeArgs().Len(); j += 1 {
						g.recordDataType(n.TypeArgs().At(j))
					}
				}
				continue
			}
			if f.Exported() {
				g.recordDataType(f.Type())
			}
		}
	}
}

func (g *gen) dataDoc(name string, named *types.Named) string {
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "/* %s (json):\n", name)
	// embedded list types marshal as a raw json array
	for i := 0; i < st.NumFields(); i += 1 {
		f := st.Field(i)
		if f.Embedded() {
			if strings.Contains(f.Type().String(), "exportedList") {
				elem := "any"
				if named, ok := types.Unalias(f.Type()).(*types.Named); ok {
					if named.TypeArgs() != nil && named.TypeArgs().Len() == 1 {
						elem = g.jsonTypeLabel(named.TypeArgs().At(0))
					}
				}
				fmt.Fprintf(&b, " *   = %s[]\n", elem)
			}
			continue
		}
		if !f.Exported() {
			continue
		}
		tag := reflectTagLookup(st.Tag(i), "json")
		jsonName := f.Name()
		optional := ""
		if tag != "" {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				jsonName = parts[0]
			}
			if slices.Contains(parts[1:], "omitempty") {
				optional = "?"
			}
		}
		fmt.Fprintf(&b, " *   %s%s: %s\n", jsonName, optional, g.jsonTypeLabel(f.Type()))
	}
	b.WriteString(" */")
	return b.String()
}

func (g *gen) jsonTypeLabel(t types.Type) string {
	t = types.Unalias(t)
	switch t := t.(type) {
	case *types.Pointer:
		return g.jsonTypeLabel(t.Elem()) + " | null"
	case *types.Slice:
		if basic, ok := types.Unalias(t.Elem()).(*types.Basic); ok && basic.Kind() == types.Uint8 {
			return "string (base64)"
		}
		return g.jsonTypeLabel(t.Elem()) + "[]"
	case *types.Map:
		return fmt.Sprintf("{[key: %s]: %s}", g.jsonTypeLabel(t.Key()), g.jsonTypeLabel(t.Elem()))
	case *types.Basic:
		switch {
		case t.Info()&types.IsBoolean != 0:
			return "boolean"
		case t.Info()&types.IsNumeric != 0:
			return "number"
		case t.Info()&types.IsString != 0:
			return "string"
		}
	case *types.Named:
		name := t.Obj().Name()
		if t.Obj().Pkg() != nil && t.Obj().Pkg().Path() == sdkPath {
			switch name {
			case "Id":
				return "string (uuid)"
			case "Time":
				return "string (rfc3339)"
			}
			if _, ok := t.Underlying().(*types.Struct); ok {
				return name
			}
			return g.jsonTypeLabel(t.Underlying())
		}
		switch t.String() {
		case "time.Time":
			return "string (rfc3339)"
		case "time.Duration":
			return "number (ns)"
		}
		return g.jsonTypeLabel(t.Underlying())
	}
	return "any"
}

func reflectTagLookup(tag string, key string) string {
	// minimal struct tag parser (reflect.StructTag without reflect)
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i += 1
		}
		tag = tag[i:]
		if tag == "" {
			break
		}
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' {
			i += 1
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i += 1
			}
			i += 1
		}
		if i >= len(tag) {
			break
		}
		value := tag[1:i]
		tag = tag[i+1:]
		if name == key {
			return value
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// output

func (g *gen) skip(symbol string, reason string) {
	g.skipped = append(g.skipped, skipRecord{symbol, reason})
}

func (g *gen) write() error {
	if err := os.MkdirAll("include", 0755); err != nil {
		return err
	}

	sortedCallbacks := make([]string, 0, len(g.callbacks))
	for name := range g.callbacks {
		sortedCallbacks = append(sortedCallbacks, name)
	}
	sort.Strings(sortedCallbacks)

	// ----- exports_gen.go / exports_gen_unix.go
	for _, unix := range []bool{false, true} {
		var b strings.Builder
		b.WriteString("// Code generated by gen/gen.go. DO NOT EDIT.\n\n")
		if unix {
			if !g.usedUnix {
				continue
			}
			b.WriteString("//go:build !windows\n\n")
		}
		b.WriteString("package main\n\n")
		b.WriteString("/*\n#include <stdint.h>\n#include <stdbool.h>\n#include \"callbacks.h\"\n*/\nimport \"C\"\n\n")
		b.WriteString("import (\n\t\"unsafe\"\n\n\t\"github.com/urnetwork/sdk\"\n)\n\n")
		b.WriteString("var _ = unsafe.Pointer(nil)\n\n")

		// callback adapters
		for _, name := range sortedCallbacks {
			info := callbackCache[name]
			if info.unixOnly != unix {
				continue
			}
			var fields []string
			for _, m := range info.methods {
				fields = append(fields, fmt.Sprintf("\t%s C.%s", m.fieldName, m.typedefName))
			}
			fields = append(fields, "\tuserData unsafe.Pointer")
			fmt.Fprintf(&b, "type %s struct {\n%s\n}\n\n", info.adapterName, strings.Join(fields, "\n"))
			for _, m := range info.methods {
				b.WriteString(m.adapter)
				b.WriteString("\n")
			}
		}

		for _, e := range g.exports {
			if e.unixOnly != unix {
				continue
			}
			b.WriteString(e.goCode)
			b.WriteString("\n")
		}

		path := "exports_gen.go"
		if unix {
			path = "exports_gen_unix.go"
		}
		formatted, err := format.Source([]byte(b.String()))
		if err != nil {
			// write the unformatted source so the compile error is inspectable
			os.WriteFile(path, []byte(b.String()), 0644)
			return fmt.Errorf("%s: format: %w", path, err)
		}
		if err := os.WriteFile(path, formatted, 0644); err != nil {
			return err
		}
	}

	// ----- callbacks.h / callbacks.c
	{
		var h strings.Builder
		h.WriteString("/* Code generated by gen/gen.go. DO NOT EDIT. */\n")
		h.WriteString("#ifndef URNETWORK_SDK_CALLBACKS_H\n#define URNETWORK_SDK_CALLBACKS_H\n\n")
		h.WriteString("#include <stdint.h>\n#include <stdbool.h>\n\n")
		var c strings.Builder
		c.WriteString("/* Code generated by gen/gen.go. DO NOT EDIT. */\n")
		c.WriteString("#include \"callbacks.h\"\n\n")
		for _, name := range sortedCallbacks {
			info := callbackCache[name]
			for _, m := range info.methods {
				h.WriteString(m.typedef)
				h.WriteString("\n")
				h.WriteString(m.shimDecl)
				h.WriteString("\n")
				c.WriteString(m.shimDef)
				c.WriteString("\n\n")
			}
		}
		h.WriteString("\n#endif\n")
		if err := os.WriteFile("callbacks.h", []byte(h.String()), 0644); err != nil {
			return err
		}
		if err := os.WriteFile("callbacks.c", []byte(c.String()), 0644); err != nil {
			return err
		}
	}

	// ----- include/urnetwork_sdk.h
	{
		var b strings.Builder
		b.WriteString("/* Code generated by gen/gen.go. DO NOT EDIT.\n")
		b.WriteString(" *\n")
		b.WriteString(" * URnetwork SDK c abi.\n")
		b.WriteString(" *\n")
		b.WriteString(" * Contract:\n")
		b.WriteString(" * - objects are opaque uint64_t handles. release every returned handle with\n")
		b.WriteString(" *   urnet_release(h). releasing a handle does not close or stop the object;\n")
		b.WriteString(" *   call the object's close/stop function first where one exists.\n")
		b.WriteString(" * - returned char* strings are owned by the caller: free with urnet_free_string.\n")
		b.WriteString(" * - const char* parameters are borrowed; the sdk copies what it needs.\n")
		b.WriteString(" * - structured data crosses as utf-8 json (see the data type reference below).\n")
		b.WriteString(" * - ids are uuid strings. times are unix epoch milliseconds, 0 = none.\n")
		b.WriteString(" * - callbacks fire on arbitrary threads; marshal to your ui thread. strings and\n")
		b.WriteString(" *   buffers passed to callbacks are only valid during the call; handles passed\n")
		b.WriteString(" *   to callbacks are owned by the receiver and must be released.\n")
		b.WriteString(" * - functions with a char** out_error parameter set a malloc'd error message on\n")
		b.WriteString(" *   failure (free with urnet_free_string). pass NULL to ignore the error text.\n")
		b.WriteString(" */\n")
		b.WriteString("#ifndef URNETWORK_SDK_H\n#define URNETWORK_SDK_H\n\n")
		b.WriteString("#include <stdint.h>\n#include <stdbool.h>\n\n")
		b.WriteString("#ifdef __cplusplus\nextern \"C\" {\n#endif\n\n")

		b.WriteString("/* ----- core ----- */\n\n")
		b.WriteString("/* the sdk version this library was built from */\n")
		b.WriteString("char* urnet_version(void);\n")
		b.WriteString("void urnet_free_string(char* s);\n")
		b.WriteString("/* release a handle. returns false if the handle was unknown. */\n")
		b.WriteString("bool urnet_release(uint64_t handle);\n")
		b.WriteString("/* number of live handles, for leak checks */\n")
		b.WriteString("int64_t urnet_live_handle_count(void);\n\n")
		b.WriteString(manualHeaderSection)

		if 0 < len(g.constants) {
			b.WriteString("/* ----- constants ----- */\n\n")
			sorted := slices.Clone(g.constants)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].cName < sorted[j].cName })
			for _, c := range sorted {
				fmt.Fprintf(&b, "#define %s %s\n", c.cName, c.literal)
			}
			b.WriteString("\n")
		}

		if 0 < len(sortedCallbacks) {
			b.WriteString("/* ----- callback types ----- */\n\n")
			for _, name := range sortedCallbacks {
				info := callbackCache[name]
				if info.unixOnly {
					continue
				}
				fmt.Fprintf(&b, "/* %s */\n", name)
				for _, m := range info.methods {
					b.WriteString(m.typedef)
					b.WriteString("\n")
				}
			}
			b.WriteString("\n")
		}

		// functions grouped
		groups := map[string][]*export{}
		var groupNames []string
		for _, e := range g.exports {
			if e.unixOnly {
				continue
			}
			if _, ok := groups[e.group]; !ok {
				groupNames = append(groupNames, e.group)
			}
			groups[e.group] = append(groups[e.group], e)
		}
		sort.Strings(groupNames)
		for _, group := range groupNames {
			fmt.Fprintf(&b, "/* ----- %s ----- */\n\n", group)
			for _, e := range groups[group] {
				b.WriteString(e.cDecl)
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}

		// unix only section
		if g.usedUnix {
			b.WriteString("/* ----- linux/unix only ----- */\n\n")
			b.WriteString("#if !defined(_WIN32)\n\n")
			for _, name := range sortedCallbacks {
				info := callbackCache[name]
				if !info.unixOnly {
					continue
				}
				fmt.Fprintf(&b, "/* %s */\n", name)
				for _, m := range info.methods {
					b.WriteString(m.typedef)
					b.WriteString("\n")
				}
			}
			b.WriteString("\n")
			for _, e := range g.exports {
				if !e.unixOnly {
					continue
				}
				b.WriteString(e.cDecl)
				b.WriteString("\n")
			}
			b.WriteString("\n#endif /* !_WIN32 */\n\n")
		}

		// data type reference
		names := make([]string, 0, len(g.dataTypes))
		for name := range g.dataTypes {
			names = append(names, name)
		}
		sort.Strings(names)
		// a data type with fields but an empty json shape is almost always a
		// behavioral type misclassified as data (fieldless structs are
		// legitimate empty markers)
		for _, name := range names {
			st, ok := g.dataTypes[name].Underlying().(*types.Struct)
			if !ok || st.NumFields() == 0 {
				continue
			}
			shaped := false
			for i := 0; i < st.NumFields(); i += 1 {
				if st.Field(i).Exported() || st.Field(i).Embedded() {
					shaped = true
					break
				}
			}
			if !shaped {
				fmt.Fprintf(os.Stderr, "warning: data type %s has an empty json shape; should it be behavioral?\n", name)
				g.skip(name, "(!) data type with empty json shape")
			}
		}
		if 0 < len(names) {
			b.WriteString("/* ----- data type reference (json shapes) ----- */\n\n")
			for _, name := range names {
				doc := g.dataDoc(name, g.dataTypes[name])
				if doc != "" {
					b.WriteString(doc)
					b.WriteString("\n\n")
				}
			}
		}

		b.WriteString("#ifdef __cplusplus\n}\n#endif\n\n#endif /* URNETWORK_SDK_H */\n")
		if err := os.WriteFile("include/urnetwork_sdk.h", []byte(b.String()), 0644); err != nil {
			return err
		}
	}

	// ----- include/urnetwork_sdk.def
	{
		names := []string{"urnet_version", "urnet_free_string", "urnet_release", "urnet_live_handle_count"}
		for _, e := range g.exports {
			if !e.unixOnly {
				names = append(names, e.cName)
			}
		}
		names = append(names, manualExports()...)
		sort.Strings(names)
		names = slices.Compact(names)
		var b strings.Builder
		b.WriteString("; Code generated by gen/gen.go. DO NOT EDIT.\n")
		b.WriteString("LIBRARY URnetworkSdk\nEXPORTS\n")
		for _, name := range names {
			b.WriteString("\t")
			b.WriteString(name)
			b.WriteString("\n")
		}
		if err := os.WriteFile("include/urnetwork_sdk.def", []byte(b.String()), 0644); err != nil {
			return err
		}
	}

	// ----- coverage_report.txt
	{
		var b strings.Builder
		b.WriteString("Code generated by gen/gen.go. DO NOT EDIT.\n")
		b.WriteString("The exported c surface, and every exported sdk symbol that is not exported with the reason.\n\n")
		fmt.Fprintf(&b, "== exported (%d) ==\n", len(g.exports))
		sortedExports := slices.Clone(g.exports)
		sort.Slice(sortedExports, func(i, j int) bool { return sortedExports[i].cName < sortedExports[j].cName })
		for _, e := range sortedExports {
			suffix := ""
			if e.unixOnly {
				suffix = " (unix only)"
			}
			fmt.Fprintf(&b, "%s <- %s%s\n", e.cName, g.cNames[e.cName], suffix)
		}
		fmt.Fprintf(&b, "\n== skipped (%d) ==\n", len(g.skipped))
		sortedSkips := slices.Clone(g.skipped)
		sort.Slice(sortedSkips, func(i, j int) bool { return sortedSkips[i].symbol < sortedSkips[j].symbol })
		for _, s := range sortedSkips {
			fmt.Fprintf(&b, "%s: %s\n", s.symbol, s.reason)
		}
		if err := os.WriteFile("coverage_report.txt", []byte(b.String()), 0644); err != nil {
			return err
		}
	}

	// ----- include/urnetwork_sdk.hpp
	if err := g.writeHpp(); err != nil {
		return err
	}

	return nil
}

// hand-written exports (exports_manual.go); keep in sync
const manualHeaderSection = `/* ----- byte buffer results (hand-written) ----- */

/* buffer-out pattern: *inout_len is always set to the needed size. the copy
 * happens and true is returned only when out is non-null and the passed
 * capacity is sufficient. */
bool urnet_decode_base58(const char* s, uint8_t* out, int32_t* inout_len);
bool urnet_decrypt_data(const char* encrypted_data_base58, const char* nonce_base58, const char* shared_secret_base58, uint8_t* out, int32_t* inout_len, char** out_error);
bool urnet_generate_shared_secret(const uint8_t* private_key, int32_t private_key_len, const uint8_t* public_key, int32_t public_key_len, uint8_t* out, int32_t* inout_len, char** out_error);

/* device local key material (stable provider identity across process starts) */
bool urnet_device_local_get_client_key_seed(uint64_t self, uint8_t* out, int32_t* inout_len);
bool urnet_device_local_get_provide_tls_certificate_pem(uint64_t self, uint8_t* out, int32_t* inout_len);
bool urnet_device_local_get_provide_tls_private_key_pem(uint64_t self, uint8_t* out, int32_t* inout_len);
bool urnet_device_local_key_material_get_client_key_seed(uint64_t self, uint8_t* out, int32_t* inout_len);
bool urnet_device_local_key_material_get_provide_tls_certificate_pem(uint64_t self, uint8_t* out, int32_t* inout_len);
bool urnet_device_local_key_material_get_provide_tls_private_key_pem(uint64_t self, uint8_t* out, int32_t* inout_len);

`

// manualExports scans the hand-written package files for //export directives
func manualExports() []string {
	var names []string
	entries, err := os.ReadDir(".")
	if err != nil {
		return names
	}
	re := regexp.MustCompile(`(?m)^//export (\w+)$`)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "exports_gen") || name == "exports_core.go" {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			names = append(names, m[1])
		}
	}
	return names
}

// ---------------------------------------------------------------------------
// names

func paramName(p *types.Var, i int) string {
	name := p.Name()
	if name == "" || name == "_" {
		return fmt.Sprintf("p%d", i)
	}
	// avoid go keywords and our locals
	switch name {
	case "self", "ok", "err", "type", "func", "range", "map", "chan", "go", "defer", "select", "var", "const", "outError":
		return name + "Param"
	}
	return name
}

func snake(name string) string {
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 && (!unicode.IsUpper(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
				b.WriteRune('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
