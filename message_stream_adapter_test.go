//go:build !sdk_mobile_bind

package sdk

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
)

// ----------------------------------------------------------------------------------------------
// the fixtures
// ----------------------------------------------------------------------------------------------

// streamAdapterTestReserver opens a real store in a fresh directory and hands back the production
// adapter over it, plus the store itself for the cases that have to look at the disk.
func streamAdapterTestReserver(t *testing.T) (messagegroup.StreamIndexReserver, *StreamStore) {
	t.Helper()
	store := streamTestOpen(t, t.TempDir())
	reserver := NewStreamIndexReserver(store)
	if reserver == nil {
		t.Fatal("NewStreamIndexReserver answered nil for a live store")
	}
	return reserver, store
}

// streamAdapterTestKey builds one StreamKey through the store's own octet flattening, so that a
// case naming a stream and a case naming a row are naming the same thing by construction.
func streamAdapterTestKey(t *testing.T, seed byte) (messagegroup.StreamKey, [][]byte) {
	t.Helper()
	parts := streamTestKeyOctets(t, seed)
	key, err := streamKeyFromOctets(parts...)
	if err != nil {
		t.Fatalf("build the stream key: %v", err)
	}
	return key, parts
}

func streamAdapterTestConcrete(t *testing.T, reserver messagegroup.StreamIndexReserver) *streamIndexReserver {
	t.Helper()
	concrete, ok := reserver.(*streamIndexReserver)
	if !ok {
		t.Fatalf("the adapter is %T, and this package's own cases reach its unexported halves", reserver)
	}
	return concrete
}

// ----------------------------------------------------------------------------------------------
// the syntax-tree helpers these gates share
// ----------------------------------------------------------------------------------------------

// streamAdapterDeclaration is one production function or method declaration, with the facts the
// gates below decide on.
type streamAdapterDeclaration struct {
	file     string
	name     string
	receiver string
	position string
	node     *ast.FuncDecl
}

func (self streamAdapterDeclaration) label() string {
	if self.receiver == "" {
		return fmt.Sprintf("%s (%s)", self.name, self.position)
	}
	return fmt.Sprintf("(%s).%s (%s)", self.receiver, self.name, self.position)
}

// streamAdapterParse parses every production file of package sdk's own directory and answers the
// file set, the parsed files and every function declaration in them.
func streamAdapterParse(t *testing.T) (*token.FileSet, map[string]*ast.File, []streamAdapterDeclaration) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fileSet := token.NewFileSet()
	parsed := map[string]*ast.File{}
	declarations := []streamAdapterDeclaration{}
	production := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		production += 1
		file, err := parser.ParseFile(fileSet, entry.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		parsed[entry.Name()] = file
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			declarations = append(declarations, streamAdapterDeclaration{
				file:     entry.Name(),
				name:     function.Name.Name,
				receiver: streamAdapterReceiverName(function),
				position: fileSet.Position(function.Pos()).String(),
				node:     function,
			})
		}
	}
	if production == 0 {
		t.Fatal("this gate read no production source, so it is holding nothing")
	}
	if len(declarations) == 0 {
		t.Fatal("this gate found no function declaration at all, so it is holding nothing")
	}
	return fileSet, parsed, declarations
}

func streamAdapterReceiverName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return ""
	}
	expression := function.Recv.List[0].Type
	if star, ok := expression.(*ast.StarExpr); ok {
		expression = star.X
	}
	if identifier, ok := expression.(*ast.Ident); ok {
		return identifier.Name
	}
	return fmt.Sprintf("%T", function.Recv.List[0].Type)
}

// streamAdapterCallsSelector answers whether the node contains a call whose function is a
// selector with one of these names, and the names it found.
func streamAdapterCallsSelector(node ast.Node, names map[string]bool) []string {
	found := map[string]bool{}
	ast.Inspect(node, func(inner ast.Node) bool {
		call, ok := inner.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && names[selector.Sel.Name] {
			found[selector.Sel.Name] = true
		}
		return true
	})
	return slices.Sorted(maps.Keys(found))
}

// streamAdapterResultTypes answers every result type of a declaration, as source text.
func streamAdapterResultTypes(function *ast.FuncDecl) []string {
	types := []string{}
	if function.Type.Results == nil {
		return types
	}
	for _, result := range function.Type.Results.List {
		text := streamAdapterTypeText(result.Type)
		count := len(result.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			types = append(types, text)
		}
	}
	return types
}

func streamAdapterTypeText(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return "*" + streamAdapterTypeText(typed.X)
	case *ast.ArrayType:
		if typed.Len == nil {
			return "[]" + streamAdapterTypeText(typed.Elt)
		}
		return "[" + streamAdapterTypeText(typed.Len) + "]" + streamAdapterTypeText(typed.Elt)
	case *ast.SelectorExpr:
		return streamAdapterTypeText(typed.X) + "." + typed.Sel.Name
	case *ast.BasicLit:
		return typed.Value
	case *ast.Ellipsis:
		return "..." + streamAdapterTypeText(typed.Elt)
	case *ast.MapType:
		return "map[" + streamAdapterTypeText(typed.Key) + "]" + streamAdapterTypeText(typed.Value)
	case *ast.InterfaceType:
		return "interface{...}"
	case *ast.FuncType:
		return "func(...)"
	case *ast.ChanType:
		return "chan " + streamAdapterTypeText(typed.Value)
	}
	return fmt.Sprintf("%T", expression)
}

// streamAdapterPackageVarNames answers every package-level variable name of package sdk's
// production files, so that a declaration that stows octets in one is visible to a gate.
func streamAdapterPackageVarNames(parsed map[string]*ast.File) map[string]bool {
	names := map[string]bool{}
	for _, file := range parsed {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range value.Names {
					names[name.Name] = true
				}
			}
		}
	}
	return names
}

// streamAdapterAssignsOutward answers the targets a declaration assigns to that OUTLIVE the call:
// a struct field, or a package-level variable. A local is neither.
func streamAdapterAssignsOutward(node ast.Node, packageVars map[string]bool) []string {
	targets := map[string]bool{}
	ast.Inspect(node, func(inner ast.Node) bool {
		assign, ok := inner.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, target := range assign.Lhs {
			switch typed := target.(type) {
			case *ast.SelectorExpr:
				targets[streamAdapterTypeText(typed)] = true
			case *ast.IndexExpr:
				targets[streamAdapterTypeText(typed.X)+"[...]"] = true
			case *ast.Ident:
				if packageVars[typed.Name] {
					targets[typed.Name] = true
				}
			}
		}
		return true
	})
	return slices.Sorted(maps.Keys(targets))
}

func streamAdapterTypeName(t *testing.T) string {
	t.Helper()
	store := streamTestOpen(t, t.TempDir())
	reserver := NewStreamIndexReserver(store)
	concrete := reflect.TypeOf(reserver)
	for concrete.Kind() == reflect.Pointer {
		concrete = concrete.Elem()
	}
	if concrete.Name() == "" {
		t.Fatal("the adapter's concrete type has no name, so a gate cannot say which declarations are its methods")
	}
	return concrete.Name()
}

// ----------------------------------------------------------------------------------------------
// Property 1 -- every flattening in sdk is the adapter's, and so is every call that carries one
// into the store.
// ----------------------------------------------------------------------------------------------

// CLASS: every production declaration of package sdk that READS A StreamKey VALUE'S FIELDS. The
// derivation is complete rather than a heuristic, and the completeness rests on a gate this file
// does not own: TestNoProductionSourceOfPackageSdkSpellsAStreamKeyFieldName holds the count of
// production sources that spell GroupId or SenderHandle at ZERO, so the only remaining way for
// production code in this package to reach a StreamKey's contents is reflection over its fields.
// The class is therefore every declaration containing a reflective field read.
//
// SCOPE, derived separately (R3): the whole of package sdk's own production files, and not the
// two files this task creates. A gate scoped to the adapter's file cannot see the second
// flattening, which is the only thing it exists to see.
//
// AND THE MATCHING IS BY SELECTOR NAME, which is an over-reach and is stated rather than hidden.
// A syntax tree without a type checker cannot tell reflect.Value.Field from reflect.Type.Field,
// or reflect.Value.Fields from strings.Fields -- and this tree has one of each: goid in trace.go
// calls strings.Fields and is carried into the class by the name alone. Over-reach can only
// WIDEN the class, never narrow it, so it cannot hide a flattening; what it costs is a member in
// the printed complement that never touched a key, which is why the complement prints what each
// member actually calls. The sound-looking narrowing -- "only declarations in files that import
// reflect" -- is NOT sound: a reflect.Value can be obtained from a helper in another file of the
// same package without ever naming the reflect package, so that filter would acquit exactly the
// declaration it most needs to convict.
//
// THE COUNT IS NOT THE FINDING. A correct implementation writes the flattening once as a helper
// and the class has one new member; a correct implementation that inlines it into each of Reserve
// and HighWater has two. Both are correct, so the gate convicts a MEMBER and never a number.
//
// THE NARROWING, AND WHY IT IS NO LONGER "INERTNESS". Until 2026-09-12 a member was excused when
// it returned no []byte and assigned to nothing that outlives the call, and the residual was
// written down as "a second flattening that neither returns its octets nor stores them ... is
// inert to this gate and IS ALSO INERT TO THE PROGRAM, because nothing outside it could observe
// the pair it built". THAT REASON IS FALSE, and it is false in ordinary Go rather than in a
// contrivance. A declaration can hand its octets out by
//
//	passing them to any call -- the callee can do anything with them;
//	sending them on a channel, which is not an assignment;
//	returning them inside a struct, an array or a named type, none of which is spelled []byte;
//	closing over them in a func literal it returns or registers.
//
// None of those four is a result of type []byte and none is an assignment to a field or a
// package-level variable, so all four were "inert" to that narrowing and every one of them is
// observable outside the declaration. It was measured: a second flattening that sends the pair
// down a package-level channel passes the old gate unchanged.
//
// WHAT REPLACES IT IS A NARROWING OVER WHAT THE DECLARATION TOUCHES RATHER THAN OVER WHERE THE
// RESULT GOES, which is sound where an escape analysis over a syntax tree is not. A flattening
// has to MOVE THE OCTETS of a key's fields; a declaration that reads a StreamKey's fields and
// moves none of them never obtained the octets at all, so it cannot have flattened them, however
// its results are shaped and wherever they go.
//
// AND THE WAYS IT CAN MOVE THEM ARE DERIVED, NOT LISTED. This narrowing used to rest on the
// sentence "reflect gives exactly four ways: Value.Bytes, Value.Slice, Value.Interface and
// reflect.Copy". That was false -- Value.Index(j).Uint() reads a [32]byte an octet at a time and
// is none of the four -- and a second flattening built on it passed the gate. Both sets are now
// read off reflect.Value's own method set at run time and every method of it must carry a
// verdict; see streamAdapterReflectValueMethods for the census, its soundness claim, its one
// exception and the stated boundary for reflect's package-level functions, which no enumeration
// can reach and which therefore fail closed.
//
// Every octet-moving member must then be a method of the adapter type or carry a RULING written
// here, and a ruling naming a declaration the tree no longer has fails too. Over-reach is the
// safe direction: a new declaration in package sdk that moves a key's octets fails this gate
// until somebody rules on it, which is a decision being asked for rather than skipped.
//
// WHAT IS STILL RESIDUAL, stated so it is not discovered later: a second flattening that reaches
// a StreamKey's contents WITHOUT reflection -- by spelling the field names -- is outside this
// class entirely, and it is held at zero by a different gate,
// TestNoProductionSourceOfPackageSdkSpellsAStreamKeyFieldName. A second flattening that moves the
// octets and then reaches the store is caught twice over, here and by the call-site gate below.

// ----------------------------------------------------------------------------------------------
// THE REFLECT CENSUS: the two sets the flattening gate narrows by, DERIVED rather than listed.
// ----------------------------------------------------------------------------------------------
//
// WHAT WAS WRONG WITH THE LIST. Until 2026-09-12 the octet-moving narrowing rested on the
// sentence "reflect gives exactly four ways to do that: Value.Bytes, Value.Slice, Value.Interface
// and reflect.Copy". That is false, and it was falsified by planting a second flattening in
// production sdk that reads the octets ONE AT A TIME --
//
//	for j := range field.Type.Len() { out[j] = byte(value.Index(j).Uint()) }
//
// -- with no unsafe, no package-level variable and no call into the store. It moved every octet
// of a StreamKey's fields into a buffer of its own and the gate was green. A four-item list
// presented as exhaustive is the shape this project has spent eleven rounds on, and the repair is
// not a fifth item.
//
// THE ENUMERATION IS THE LANGUAGE'S. reflect.Value's method set is read off
// reflect.TypeOf(reflect.Value{}) at run time, and every method of it must carry a ruling below.
// A method this census does not name FAILS the gate rather than being skipped, so the day a Go
// release adds one -- Go 1.26 added Fields, Methods, Seq and Seq2, three of which are new ways to
// reach a struct's field values -- it arrives as a decision to make and not as a silent hole.
// Nothing here is a list of "the ways": the list is the method set, and what is written by hand
// is the VERDICT on each name, which is the part no derivation can supply.
//
// THE TWO QUESTIONS, and they are different questions:
//
//	reads -- called on a struct's Value, can this yield one of its FIELDS as a Value? This is
//	         what puts a declaration in the class at all. It used to be three names; the method
//	         set says six.
//
//	moves -- can this put a field's octets somewhere that is NOT another reflect.Value: into
//	         ordinary Go storage the declaration can keep, pass, send, return or close over?
//	         This is the narrowing, and the complement it removes is printed.
//
// THE SOUNDNESS CLAIM, stated so it can be attacked: every path by which a key field's octets can
// leave a declaration passes through at least one method ruled moves. The methods ruled NOT
// movers are exactly those that
//
//	(a) answer a DESCRIPTION of the value -- a bool, an int, a Kind, a Type -- and never its
//	    contents; or
//	(b) yield another reflect.Value, so the octets are still inside the graph and a mover is
//	    still owed before they can leave it.
//
// Navigation is (b): Field, Index, Slice, Elem, Addr and MapIndex all hand back a Value, which is
// why the planted per-octet flattening is convicted at Uint and not at Index. Scalar readers are
// movers whether or not a [32]byte can reach their Kind today, because the class is StreamKey's
// FIELDS and StreamKey's field set is connect's to change.
//
// THE ONE EXCEPTION, AND ITS BOUNDARY, because it is an exception and not an oversight.
// Value.String is ruled NOT a mover. On a Value whose Kind is String it answers the contents, and
// Kind String is reachable from a [32]byte -- Slice it, Convert the slice to string, read it --
// so the honest ruling would be "mover". It is not ruled one because THIS GATE MATCHES SELECTOR
// NAMES AND reflect.Type HAS A String METHOD TOO: ruling it a mover convicts every declaration
// that formats a type into an error message, which is both of the declarations the octet-moving
// narrowing currently removes, and an empty complement is a narrowing that has stopped narrowing.
// What keeps the exception sound is that Convert IS ruled a mover: Kind String is not reachable
// from an array of uint8 without Convert or Interface, and both convict one call earlier.
// THE RESIDUAL, stated rather than discovered: if connect ever gives StreamKey a field whose Kind
// is already String, a second flattening could read it with Value.String alone and this gate
// would not see it. Every field of StreamKey today is an array of uint8 -- the adapter refuses
// anything else, and TestAKeyOfTheWrongWidthIsRefusedAtTheBoundary holds that -- so the residual
// is empty on this tree and it is not empty by construction.
//
// AND THE HALF THAT CANNOT BE ENUMERATED AT ALL, so it is a STATED BOUNDARY AND IT FAILS CLOSED:
// reflect's package-level FUNCTIONS. Go has no reflection over a package's functions, so there is
// no method set to read and no completeness check to run in the other direction. The gate
// therefore treats EVERY call spelled reflect.Something(...) inside a class member as an octet
// mover UNLESS that name carries a ruling below, and reports the unruled name. A function this
// census has never heard of convicts; it does not pass.

type streamAdapterReflectRuling struct {
	// reads is: called on a struct's Value, can this yield one of its FIELDS as a Value?
	reads bool
	// moves is: can this put a field's octets somewhere that is not another reflect.Value?
	moves bool
	why   string
}

// streamAdapterReflectValueMethods must be TOTAL over reflect.Value's exported method set, and
// streamAdapterReflectCensus holds it to that in both directions at run time.
var streamAdapterReflectValueMethods = map[string]streamAdapterReflectRuling{
	"Addr":            {why: "yields a pointer Value naming the field's storage -- still a reflect.Value, so a mover is still owed"},
	"Bool":            {moves: true, why: "a scalar read of the value's contents"},
	"Bytes":           {moves: true, why: "answers the octets as an ordinary []byte"},
	"Call":            {moves: true, why: "calls a function Value with argument Values, and the callee keeps whatever it is handed"},
	"CallSlice":       {moves: true, why: "Call with a variadic final argument; the callee keeps what it is handed"},
	"CanAddr":         {why: "a bool about the Value"},
	"CanComplex":      {why: "a bool about the Value's Kind"},
	"CanConvert":      {why: "a bool about the Value's Kind"},
	"CanFloat":        {why: "a bool about the Value's Kind"},
	"CanInt":          {why: "a bool about the Value's Kind"},
	"CanInterface":    {why: "a bool about the Value"},
	"CanSet":          {why: "a bool about the Value"},
	"CanUint":         {why: "a bool about the Value's Kind"},
	"Cap":             {why: "an int describing the value"},
	"Clear":           {why: "zeroes a map or a slice; it writes zeroes and answers nothing, so no source value's octets pass through it"},
	"Close":           {why: "closes a channel and answers nothing"},
	"Comparable":      {why: "a bool about the Value's type"},
	"Complex":         {moves: true, why: "a scalar read of the value's contents"},
	"Convert":         {moves: true, why: "THE ONE NAVIGATION METHOD RULED A MOVER. It is the only way to change a Value's Kind, and it is what makes Kind String -- whose reader is the exception above -- reachable from an array of uint8"},
	"Elem":            {why: "dereferences a pointer or unwraps an interface into another reflect.Value"},
	"Equal":           {why: "a bool comparing two Values"},
	"Field":           {reads: true, why: "a struct's field, by position, as a Value"},
	"FieldByIndex":    {reads: true, why: "a struct's field, by index path, as a Value"},
	"FieldByIndexErr": {reads: true, why: "FieldByIndex answering an error instead of panicking on a nil embedded pointer"},
	"FieldByName":     {reads: true, why: "a struct's field, by name, as a Value"},
	"FieldByNameFunc": {reads: true, why: "a struct's field, by a predicate over names, as a Value"},
	"Fields":          {reads: true, why: "iterates a struct's fields, yielding each as a Value. Added in Go 1.26, and invisible to the three-name class this gate used to carry"},
	"Float":           {moves: true, why: "a scalar read of the value's contents"},
	"Grow":            {why: "increases a slice's capacity; it moves no source value's octets"},
	"Index":           {why: "one element of an array, slice or string as a Value. The per-octet flattening that falsified the old four-item list goes through here, and is convicted one call later at Uint"},
	"Int":             {moves: true, why: "a scalar read of the value's contents"},
	"Interface":       {moves: true, why: "answers the value as an ordinary any, contents and all"},
	"InterfaceData":   {moves: true, why: "answers the interface's word pair, which is a pointer to the contents"},
	"IsNil":           {why: "a bool about the Value"},
	"IsValid":         {why: "a bool about the Value"},
	"IsZero":          {why: "a bool about the Value"},
	"Kind":            {why: "a Kind describing the value"},
	"Len":             {why: "an int describing the value"},
	"MapIndex":        {why: "a map entry as another reflect.Value"},
	"MapKeys":         {why: "a map's keys as reflect.Values"},
	"MapRange":        {why: "an iterator over reflect.Values"},
	"Method":          {why: "a method as another reflect.Value"},
	"MethodByName":    {why: "a method as another reflect.Value"},
	"Methods":         {why: "iterates a type's methods as reflect.Values. Added in Go 1.26"},
	"NumField":        {why: "an int describing the type"},
	"NumMethod":       {why: "an int describing the type"},
	"OverflowComplex": {why: "a bool about a candidate value"},
	"OverflowFloat":   {why: "a bool about a candidate value"},
	"OverflowInt":     {why: "a bool about a candidate value"},
	"OverflowUint":    {why: "a bool about a candidate value"},
	"Pointer":         {moves: true, why: "answers the data pointer as a uintptr, which names the octets"},
	"Recv":            {why: "receives from a channel into another reflect.Value; it takes nothing out of the receiver"},
	"Seq":             {why: "iterates a value's elements as reflect.Values"},
	"Seq2":            {why: "iterates a value's index/element or key/value pairs as reflect.Values"},
	"Send":            {moves: true, why: "sends a Value on a channel, and a channel is ordinary storage the declaration and its readers keep"},
	"Set":             {moves: true, why: "copies one Value's contents into another's storage, which is the caller's variable"},
	"SetBool":         {moves: true, why: "writes contents into a Value's storage; the inverse flattening's direction, and this gate convicts both"},
	"SetBytes":        {moves: true, why: "writes octets into a Value's storage"},
	"SetCap":          {why: "changes a slice header's capacity; it moves no contents"},
	"SetComplex":      {moves: true, why: "writes contents into a Value's storage"},
	"SetFloat":        {moves: true, why: "writes contents into a Value's storage"},
	"SetInt":          {moves: true, why: "writes contents into a Value's storage"},
	"SetIterKey":      {moves: true, why: "writes a map iterator's current key into a Value's storage"},
	"SetIterValue":    {moves: true, why: "writes a map iterator's current value into a Value's storage"},
	"SetLen":          {why: "changes a slice header's length; it moves no contents"},
	"SetMapIndex":     {moves: true, why: "writes a Value into a map the caller keeps"},
	"SetPointer":      {moves: true, why: "writes an unsafe.Pointer into a Value's storage"},
	"SetString":       {moves: true, why: "writes contents into a Value's storage"},
	"SetUint":         {moves: true, why: "writes contents into a Value's storage"},
	"SetZero":         {why: "writes the zero value; no source value's octets pass through it"},
	"Slice":           {why: "a sub-slice of an array or slice as another reflect.Value. It ALIASES the field's octets, and it was on the old four-item list, but on its own it hands nothing out: Bytes or Interface is still owed"},
	"Slice3":          {why: "Slice with an explicit capacity; another reflect.Value"},
	"String":          {why: "THE EXCEPTION, AND ITS BOUNDARY IS IN THE HEADER ABOVE. On Kind String it answers the contents, and Kind String is reachable from an array of uint8 only through Convert or Interface, both of which are movers. It is ruled here rather than as a mover because reflect.Type has a String method too and this gate matches names, so ruling it a mover empties the complement"},
	"TryRecv":         {why: "a non-blocking Recv; it takes nothing out of the receiver"},
	"TrySend":         {moves: true, why: "a non-blocking Send; the Value leaves on a channel"},
	"Type":            {why: "a reflect.Type describing the value"},
	"Uint":            {moves: true, why: "a scalar read of the value's contents. THIS IS THE ONE THE OLD FOUR-ITEM LIST MISSED: Index(j).Uint() reads a [32]byte one octet at a time and assembles the pair with neither Bytes, Slice, Interface nor Copy"},
	"UnsafeAddr":      {moves: true, why: "answers the address of the octets as a uintptr"},
	"UnsafePointer":   {moves: true, why: "answers a pointer to the octets"},
}

// streamAdapterReflectFunctions is the STATED BOUNDARY: reflect's package-level functions cannot
// be enumerated, so this table can only be checked in one direction -- a name it does not hold
// convicts. It is not a claim to be complete; it is a claim that incompleteness fails closed.
var streamAdapterReflectFunctions = map[string]streamAdapterReflectRuling{
	"Append":      {moves: true, why: "appends Values to a slice the caller keeps"},
	"AppendSlice": {moves: true, why: "appends one slice Value's contents to another"},
	"Copy":        {moves: true, why: "copies one Value's contents into another's storage"},
	"DeepEqual":   {why: "a bool comparing two values"},
	"Indirect":    {why: "dereferences into another reflect.Value"},
	"MakeChan":    {why: "an empty channel Value"},
	"MakeMap":     {why: "an empty map Value"},
	"MakeSlice":   {why: "a zeroed slice Value"},
	"New":         {why: "a zeroed addressable Value of a type; it reads nothing"},
	"NewAt":       {moves: true, why: "builds a Value over memory named by an unsafe.Pointer"},
	"PointerTo":   {why: "a reflect.Type"},
	"Select":      {moves: true, why: "can carry a Value out on a send case"},
	"SliceAt":     {moves: true, why: "builds a slice Value over memory named by an unsafe.Pointer"},
	"TypeFor":     {why: "a reflect.Type"},
	"TypeOf":      {why: "a reflect.Type"},
	"ValueOf":     {why: "wraps an ordinary value INTO the reflect graph; it takes nothing out of one"},
	"Zero":        {why: "a zero Value of a type"},
}

// streamAdapterReflectCensus holds the method table against reflect.Value's own method set, in
// both directions, and answers the two derived sets plus the complement the moves narrowing
// removes.
func streamAdapterReflectCensus(t *testing.T) (map[string]bool, map[string]bool, []string) {
	t.Helper()
	valueType := reflect.TypeOf(reflect.Value{})
	onTheType := map[string]bool{}
	for i := range valueType.NumMethod() {
		onTheType[valueType.Method(i).Name] = true
	}
	if len(onTheType) == 0 {
		t.Fatal("reflect.Value answered no exported method, so this census read nothing and the narrowing below is derived from nothing")
	}
	unruled := []string{}
	for name := range onTheType {
		if _, ruled := streamAdapterReflectValueMethods[name]; !ruled {
			unruled = append(unruled, name)
		}
	}
	stale := []string{}
	for name := range streamAdapterReflectValueMethods {
		if !onTheType[name] {
			stale = append(stale, name)
		}
	}
	slices.Sort(unruled)
	slices.Sort(stale)
	if len(unruled) != 0 {
		t.Errorf(
			"reflect.Value has %d exported method(s) streamAdapterReflectValueMethods does not rule on: %v. Rule each one: can it yield a struct FIELD as a Value, and can it put that field's octets somewhere that is not another reflect.Value? Until then this gate's narrowing is a list again, and a list is what the per-octet flattening walked past",
			len(unruled), unruled,
		)
	}
	if len(stale) != 0 {
		t.Errorf("streamAdapterReflectValueMethods rules on %d name(s) reflect.Value does not declare: %v", len(stale), stale)
	}
	reads := map[string]bool{}
	moves := map[string]bool{}
	complement := []string{}
	for name, ruling := range streamAdapterReflectValueMethods {
		if ruling.reads {
			reads[name] = true
		}
		if ruling.moves {
			moves[name] = true
			continue
		}
		complement = append(complement, fmt.Sprintf("reflect.Value.%s -- %s", name, ruling.why))
	}
	for name, ruling := range streamAdapterReflectFunctions {
		if ruling.moves {
			moves[name] = true
			continue
		}
		complement = append(complement, fmt.Sprintf("reflect.%s -- %s", name, ruling.why))
	}
	slices.Sort(complement)
	if len(reads) == 0 {
		t.Fatal("no method of reflect.Value is ruled as reading a struct's fields, so the flattening gate's class is empty")
	}
	if len(moves) == 0 {
		t.Fatal("no method of reflect.Value is ruled as moving octets, so the flattening gate would convict nothing")
	}
	t.Logf("REFLECT CENSUS: reflect.Value declares %d exported method(s); every one of them carries a ruling", len(onTheType))
	t.Logf("  the %d that can yield a struct's FIELD as a Value: %v", len(reads), slices.Sorted(maps.Keys(reads)))
	t.Logf("  the %d that can put octets outside the reflect.Value graph (with reflect's own functions): %v", len(moves), slices.Sorted(maps.Keys(moves)))
	return reads, moves, complement
}

// streamAdapterReflectPackageCalls answers every call spelled <reflect>.Name(...) inside a node,
// where <reflect> is the local name of the reflect import in that file.
func streamAdapterReflectPackageCalls(node ast.Node, local string) []string {
	found := map[string]bool{}
	if local == "" {
		return nil
	}
	ast.Inspect(node, func(inner ast.Node) bool {
		call, ok := inner.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok || identifier.Name != local {
			return true
		}
		found[selector.Sel.Name] = true
		return true
	})
	return slices.Sorted(maps.Keys(found))
}

// streamAdapterImportLocalNames answers, per production file, the local name the reflect package
// is imported under -- read from each file's own import spec, so an aliased import cannot walk
// past the boundary check.
func streamAdapterImportLocalNames(parsed map[string]*ast.File, path string) map[string]string {
	local := map[string]string{}
	for name, file := range parsed {
		for _, imported := range file.Imports {
			quoted, err := strconv.Unquote(imported.Path.Value)
			if err != nil || quoted != path {
				continue
			}
			if imported.Name != nil {
				local[name] = imported.Name.Name
			} else {
				local[name] = path[strings.LastIndex(path, "/")+1:]
			}
		}
	}
	return local
}

// streamAdapterOctetMovingRulings is the one place a declaration outside the adapter is excused
// from being a second flattening, and each excuse is a sentence rather than a name on a list.
var streamAdapterOctetMovingRulings = map[string]string{
	"streamRowIdentityOf":         "the store's ROW IDENTITY derivation. It moves a field's octets straight into a SHA-256, one field at a time, and what leaves it is a hex digest; it never assembles section 8.2's positional pair and no caller can recover the pair from what it returns. It is the derivation the adapter's flattening is keyed AGAINST rather than a second copy of it",
	"setMessageServerRequestBody": "NOT A StreamKey FLATTENING AT ALL, and this is the first false positive the name-matched half of this class has produced. The two names it trips on are protobuf's, not reflect's: `Fields` is protoreflect.OneofDescriptor.Fields, which enumerates the arms of §4.3's request body oneof, and `Set` is protoreflect.Message.Set, which puts a body into the arm that carries its type. No messagegroup.StreamKey, no stream, no row and no index is in reach of it -- it moves a request body's octets into a protobuf message and nothing else. The gate matches SELECTOR NAMES because a receiver's type is not knowable off the syntax tree, so protoreflect's Fields/Set and reflect.Value's Fields/Set are one name to it; over-reach is the safe direction and this is what over-reach costs, paid once, in a sentence, rather than by narrowing the class",
	"messageCapabilityValue":      "THE SECOND false positive of the name-matched half, and the same collision as setMessageServerRequestBody rather than a new one. It reads ONE advertised bound off a protocol.Capabilities by its descriptor name: `Fields` is protoreflect.MessageDescriptor.Fields and `Int`/`Uint` are protoreflect.Value.Int/Uint, none of them reflect.Value's. What it answers is a uint64 -- a byte budget §4.3.1 advertised -- and no messagegroup.StreamKey, no stream, no row and no index is in reach of it. Recorded here rather than fixed by narrowing the class, because the gate matches SELECTOR NAMES on purpose: a receiver's type is not knowable off the syntax tree, and over-reach is the safe direction. The cost is one sentence per collision and this is the second one",
	"streamKeyFromOctets":         "THE INVERSE flattening -- section 8.2's positional pair back onto StreamKey -- which section 8.2 puts in the store, beside the two methods that take that pair. It moves octets INTO a key rather than out of one, so it cannot be a second derivation of which row a stream's indices land in: it consumes the one the adapter produced",
}

func TestEveryStreamKeyFlatteningInPackageSdkIsTheAdapters(t *testing.T) {
	adapterType := streamAdapterTypeName(t)
	_, parsed, declarations := streamAdapterParse(t)
	packageVars := streamAdapterPackageVarNames(parsed)
	reflectiveReads, octetMovers, reflectComplement := streamAdapterReflectCensus(t)
	reflectLocal := streamAdapterImportLocalNames(parsed, "reflect")

	t.Logf("SCOPE: %d production file(s) of package sdk, %d function declaration(s), %d importing reflect", len(parsed), len(declarations), len(reflectLocal))
	t.Logf("CLASS: declarations containing a reflective field read %v -- read off reflect.Value's method set, not written down", slices.Sorted(maps.Keys(reflectiveReads)))
	t.Logf("NARROWED TO: of those, the ones that also MOVE the octets %v", slices.Sorted(maps.Keys(octetMovers)))
	t.Logf("COMPLEMENT the reflect census removed (%d operation(s) that cannot put a field's octets outside the reflect.Value graph):", len(reflectComplement))
	for _, line := range reflectComplement {
		t.Logf("    %s", line)
	}
	if len(reflectComplement) == 0 {
		t.Error("the reflect census excluded no operation at all, so the octet-moving narrowing is not narrowing")
	}
	t.Logf("the adapter type, read by calling the producer rather than written down: %s", adapterType)

	members := 0
	readsOnly := []string{}
	adapters := []string{}
	ruled := []string{}
	seen := map[string]bool{}
	for _, declaration := range declarations {
		reads := streamAdapterCallsSelector(declaration.node, reflectiveReads)
		if len(reads) == 0 {
			continue
		}
		members += 1
		moves := streamAdapterCallsSelector(declaration.node, octetMovers)
		// THE STATED BOUNDARY, FAILING CLOSED. reflect's package-level functions cannot be
		// enumerated, so a call into the reflect package that this census has never heard of
		// is treated as an octet mover AND reported, rather than passed over.
		for _, called := range streamAdapterReflectPackageCalls(declaration.node, reflectLocal[declaration.file]) {
			ruling, ruled := streamAdapterReflectFunctions[called]
			if !ruled {
				t.Errorf(
					"%s reads a StreamKey's fields and calls reflect.%s, which streamAdapterReflectFunctions does not rule on. Go has no reflection over a package's functions, so this half of the narrowing cannot be enumerated and it fails CLOSED: rule reflect.%s -- can it put a field's octets somewhere that is not another reflect.Value? -- rather than leaving the gate to guess",
					declaration.label(), called, called,
				)
				moves = append(moves, "reflect."+called)
				continue
			}
			if ruling.moves && !slices.Contains(moves, called) {
				moves = append(moves, called)
			}
		}
		slices.Sort(moves)
		results := streamAdapterResultTypes(declaration.node)
		outward := streamAdapterAssignsOutward(declaration.node, packageVars)
		if len(moves) == 0 {
			readsOnly = append(readsOnly, fmt.Sprintf("%s reads %v, moves no octets (results %v)", declaration.label(), reads, results))
			continue
		}
		seen[declaration.name] = true
		switch {
		case declaration.receiver == adapterType:
			adapters = append(adapters, fmt.Sprintf("%s reads %v, moves %v, results %v", declaration.label(), reads, moves, results))
		case streamAdapterOctetMovingRulings[declaration.name] != "":
			ruled = append(ruled, fmt.Sprintf("%s moves %v, results %v, assigns outward %v\n      RULING: %s",
				declaration.label(), moves, results, outward, streamAdapterOctetMovingRulings[declaration.name]))
		default:
			t.Errorf(
				"%s reads a StreamKey's fields %v and MOVES their octets %v, and it is neither a method of %s nor a declaration streamAdapterOctetMovingRulings rules on (results %v, outward assignments %v). That is a SECOND flattening of one mapping: two derivations of which row a stream's indices land in, and the day they disagree the second stream is handed indices the first has already spent. Where the octets GO is not the question -- a call argument, a channel send, a struct field of a result and a closure capture all carry them out of here and none of them is a []byte result",
				declaration.label(),
				reads,
				moves,
				adapterType,
				results,
				outward,
			)
		}
	}
	if members == 0 {
		t.Fatal("the class is empty, so this gate read no reflective field access at all and is holding nothing")
	}
	t.Logf("CLASS MEMBERS: %d", members)
	t.Logf("  the adapter's (%d):", len(adapters))
	for _, line := range adapters {
		t.Logf("    %s", line)
	}
	t.Logf("  RULED (%d) -- these move the octets and are not the adapter's:", len(ruled))
	for _, line := range ruled {
		t.Logf("    %s", line)
	}
	t.Logf("  COMPLEMENT the octet-moving narrowing removed (%d) -- these call a field-reading name and move no octets:", len(readsOnly))
	for _, line := range readsOnly {
		t.Logf("    %s", line)
	}
	if len(adapters) == 0 {
		t.Errorf("no method of %s reads a StreamKey's fields and moves their octets, so the flattening this task produces is not in the class this gate reads and the gate is holding nothing", adapterType)
	}
	if len(readsOnly) == 0 {
		t.Error("the complement is empty, so the octet-moving narrowing removed no declaration at all and this gate is not the gate it says it is")
	}
	stale := []string{}
	for name := range streamAdapterOctetMovingRulings {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	if len(stale) != 0 {
		t.Errorf("streamAdapterOctetMovingRulings excuses %d declaration(s) this tree does not have as octet-moving members: %v; an excuse nothing matches is an excuse the next declaration of that name inherits for free", len(stale), stale)
	}
}

// The second half of Property 1: a flattening that never reaches the store is inert, so the gate
// above is paired with one over the CALL, and the call sites are where a second flattening
// actually does its damage.
//
// CLASS: the methods of *StreamStore whose SHAPE is section 8.2's key-shaped pair --
// (groupId, senderHandle []byte) answering (uint64, error) -- read off the type's own method set
// through reflection rather than written down, so a third method of that shape added tomorrow is
// in the class on the day it arrives.
// SCOPE, derived separately: every call expression in package sdk's production files.
func TestTheAdapterIsTheOnlyProductionCallerOfTheStoresKeyShapedMethods(t *testing.T) {
	adapterType := streamAdapterTypeName(t)
	storeType := reflect.TypeOf((*StreamStore)(nil))
	octets := reflect.TypeOf([]byte(nil))
	keyShaped := map[string]bool{}
	complement := []string{}
	for i := range storeType.NumMethod() {
		method := storeType.Method(i)
		shape := method.Type
		matches := shape.NumIn() == 3 && shape.In(1) == octets && shape.In(2) == octets &&
			shape.NumOut() == 2 && shape.Out(0).Kind() == reflect.Uint64 &&
			shape.Out(1) == reflect.TypeOf((*error)(nil)).Elem()
		if matches {
			keyShaped[method.Name] = true
			continue
		}
		complement = append(complement, fmt.Sprintf("%s%s", method.Name, shape.String()))
	}
	t.Logf("CLASS: %d key-shaped method(s) of *StreamStore, read off the method set: %v",
		len(keyShaped), slices.Sorted(maps.Keys(keyShaped)))
	t.Logf("COMPLEMENT the shape narrowing removed (%d exported method(s) of *StreamStore):", len(complement))
	for _, line := range complement {
		t.Logf("    %s", line)
	}
	if len(keyShaped) == 0 {
		t.Fatal("no method of *StreamStore has section 8.2's key shape, so this gate is holding nothing")
	}
	if len(complement) == 0 {
		t.Error("the complement is empty, so the shape narrowing removed no method at all and this gate is not the gate it says it is")
	}

	_, _, declarations := streamAdapterParse(t)
	callers := 0
	for _, declaration := range declarations {
		calls := streamAdapterCallsSelector(declaration.node, keyShaped)
		if len(calls) == 0 {
			continue
		}
		callers += 1
		if declaration.receiver != adapterType {
			t.Errorf(
				"%s calls the store's key-shaped method(s) %v and is not a method of %s; the two []byte it passes came from somewhere, and the only place in this package that may turn a StreamKey into them is the adapter",
				declaration.label(),
				calls,
				adapterType,
			)
			continue
		}
		t.Logf("  caller: %s calls %v", declaration.label(), calls)
	}
	if callers == 0 {
		t.Error("no production declaration calls the store's key-shaped methods, so nothing in this build reaches the durable reservation and this gate is holding nothing")
	}
}

// The third half, and it is the sentence the adapter's header makes: the adapter is the ONLY place
// a store failure becomes a messagegroup sentinel.
//
// CLASS: every production reference to an exported identifier of connect/messagegroup whose name
// begins with Err. The package's local name is read from each file's own import spec rather than
// assumed, so an aliased import cannot walk past this.
// SCOPE, derived separately: every production file of package sdk.
func TestTheAdapterIsTheOnlyProductionSourceThatNamesAMessagegroupSentinel(t *testing.T) {
	adapterType := streamAdapterTypeName(t)
	fileSet, parsed, declarations := streamAdapterParse(t)

	local := map[string]string{}
	for name, file := range parsed {
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil || path != "github.com/urnetwork/connect/messagegroup" {
				continue
			}
			if imported.Name != nil {
				local[name] = imported.Name.Name
			} else {
				local[name] = "messagegroup"
			}
		}
	}
	t.Logf("SCOPE: %d production file(s); %d import connect/messagegroup: %v",
		len(parsed), len(local), slices.Sorted(maps.Keys(local)))
	if len(local) == 0 {
		t.Fatal("no production file imports connect/messagegroup, so this gate read no file in which a messagegroup sentinel could appear")
	}

	sentinelSites := map[string][]string{}
	otherSites := map[string]bool{}
	for name, file := range parsed {
		packageName, ok := local[name]
		if !ok {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			identifier, ok := selector.X.(*ast.Ident)
			if !ok || identifier.Name != packageName {
				return true
			}
			if strings.HasPrefix(selector.Sel.Name, "Err") {
				position := fileSet.Position(selector.Pos()).String()
				sentinelSites[selector.Sel.Name] = append(sentinelSites[selector.Sel.Name], position)
				return true
			}
			otherSites[selector.Sel.Name] = true
			return true
		})
	}
	t.Logf("CLASS: %d distinct messagegroup sentinel(s) named in production: %v",
		len(sentinelSites), slices.Sorted(maps.Keys(sentinelSites)))
	t.Logf("COMPLEMENT the Err narrowing removed (%d other messagegroup identifier(s) named in production): %v",
		len(otherSites), slices.Sorted(maps.Keys(otherSites)))
	if len(sentinelSites) == 0 {
		t.Fatal("no production source names a messagegroup sentinel, so nothing maps a store failure onto one and a store refusal reaches SenderRatchet.Next classified as retryable")
	}
	if len(otherSites) == 0 {
		t.Error("the complement is empty, which means the Err narrowing removed nothing and this gate is not the gate it says it is")
	}

	for _, declaration := range declarations {
		named := map[string]bool{}
		ast.Inspect(declaration.node, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			identifier, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if identifier.Name == local[declaration.file] && strings.HasPrefix(selector.Sel.Name, "Err") {
				named[selector.Sel.Name] = true
			}
			return true
		})
		if len(named) == 0 {
			continue
		}
		if declaration.receiver != adapterType {
			t.Errorf(
				"%s names the messagegroup sentinel(s) %v and is not a method of %s; a second mapping site is a second answer to the one question SenderRatchet.Next asks, and the two only have to disagree once",
				declaration.label(),
				slices.Sorted(maps.Keys(named)),
				adapterType,
			)
			continue
		}
		t.Logf("  mapping site: %s names %v", declaration.label(), slices.Sorted(maps.Keys(named)))
	}
}

// ----------------------------------------------------------------------------------------------
// Property 2 -- the adapter copies the key at the boundary.
// ----------------------------------------------------------------------------------------------

// StreamKey is comparable deliberately twice over, and section 8.2's []byte pair is neither
// comparable nor immutable. Nothing outside the adapter may come to hold a slice that names a
// row's identity, and no two calls may hand out the same array: a row's identity is which indices
// a stream has already spent.
func TestTheAdapterCopiesTheKeyAtTheBoundary(t *testing.T) {
	reserver, store := streamAdapterTestReserver(t)
	concrete := streamAdapterTestConcrete(t, reserver)
	key, parts := streamAdapterTestKey(t, 0x81)
	rowName := streamRowName(key)

	first, err := concrete.keyOctets(key)
	if err != nil {
		t.Fatalf("the flattening refused a well formed key: %v", err)
	}
	second, err := concrete.keyOctets(key)
	if err != nil {
		t.Fatalf("the flattening refused a well formed key on its second call: %v", err)
	}
	if len(first) != len(parts) {
		t.Fatalf("the flattening produced %d part(s), want %d", len(first), len(parts))
	}
	for i := range first {
		if !bytes.Equal(first[i], parts[i]) {
			t.Errorf("part %d is %x, want %x; the adapter's flattening and the store's must be inverses", first[i], parts[i], i)
		}
		if &first[i][0] == &second[i][0] {
			t.Errorf("part %d of two calls shares one backing array at %p; a buffer hoisted out of the call makes two flattenings alias, which is the same defect as not copying at all", i, &first[i][0])
		}
	}
	for i := range first {
		for j := range first {
			if i != j && &first[i][0] == &first[j][0] {
				t.Errorf("parts %d and %d of one call share a backing array", i, j)
			}
		}
	}

	// the observable: scribbling on what the flattening handed back must not move the row a
	// later call names.
	if index, err := reserver.Reserve(key); err != nil || index != 1 {
		t.Fatalf("the first allocation answered (%d, %v), want (1, nil)", index, err)
	}
	for i := range first {
		for j := range first[i] {
			first[i][j] ^= 0xFF
		}
	}
	if index, err := reserver.Reserve(key); err != nil || index != 2 {
		t.Fatalf("the allocation after the caller scribbled on the flattening's output answered (%d, %v), want (2, nil)", index, err)
	}
	length := streamTestRowLength(t, filepath.Join(store.rowDir, rowName))
	if length != 2*streamRecordWidth {
		t.Errorf("row %s is %d octets, want %d; both allocations must have landed on the SAME row, and a moved row is two ladders under one key", rowName, length, 2*streamRecordWidth)
	}
	entries, err := os.ReadDir(store.rowDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the row directory holds %d row(s) %v, want exactly 1; a second row is the scribble having moved the stream", len(entries), names)
	}
}

// ----------------------------------------------------------------------------------------------
// Property 3 -- the sentinel mapping, and the class it must be total over.
// ----------------------------------------------------------------------------------------------

// ----------------------------------------------------------------------------------------------
// THE SENTINEL CLASS, AND WHY IT IS NO LONGER A SPELLING
// ----------------------------------------------------------------------------------------------
//
// The class this gate must be total over is "A PACKAGE-LEVEL VALUE OF PACKAGE SDK THAT IS AN
// ERROR". The previous derivation was total over one SPELLING of that -- an initialiser that is
// literally errors.New("<string literal>") -- and a ninth sentinel spelled fmt.Errorf(...), or
// errors.New(someConst), or &someErrorType{}, or a plain `var x error = ...` was INVISIBLE to it.
// An invisible sentinel carries no ruling, and classify forwards an unruled error as TRANSIENT,
// which is exactly the unbounded retry the adapter exists to stop. Two of three ordinary
// spellings were planted against the old gate and forwarded silently; it caught one.
//
// THE SCOPE STAYS THE WHOLE PACKAGE. The previous derivation was right to take every production
// file of package sdk rather than a name prefix, and that instinct is kept. What is widened is
// the spelling, and it is widened by taking spelling out of the derivation entirely:
//
//	the ENUMERATION is over package-level var NAMES, off the syntax tree. No initialiser
//	shape can hide a name, because a var declaration must spell the name it declares.
//
//	the CLASSIFICATION is over the DECLARED TYPE behind that name, at run time, through a
//	pointer: a type either implements error or it does not, and reflect answers that. No
//	list of spellings is consulted by anything.
//
// The two are joined by streamAdapterPackageVarCensus, which is the one thing here that must be
// maintained by hand -- Go has no reflection over a package's variables, so a name cannot be
// turned into a value any other way -- and the completeness check below is what stops it going
// stale IN BOTH DIRECTIONS: a name the tree has and the census does not is a failure, and so is a
// name the census has and the tree does not.

// streamAdapterErrorType is the error interface itself, read off the language rather than spelled.
var streamAdapterErrorType = reflect.TypeOf((*error)(nil)).Elem()

// streamAdapterPackageVar is one package-level variable of package sdk, reached BY POINTER so
// that nothing here copies the value it names: one of them is a sync.Mutex, and a census that
// copied it would be one go vet refuses to let exist.
type streamAdapterPackageVar struct {
	declared reflect.Type
	value    reflect.Value
}

// streamAdapterPackageVarOf takes the ADDRESS of a package-level variable and answers its
// DECLARED type -- not the dynamic type of whatever is in it. For `var e = errors.New("x")` that
// is the interface type error, and error implements error; for `var e = &rowError{}` it is
// *rowError, which implements error too. Either way "is this an error" is answered by the type
// system rather than by reading an initialiser.
func streamAdapterPackageVarOf[T any](pointer *T) streamAdapterPackageVar {
	return streamAdapterPackageVar{
		declared: reflect.TypeOf((*T)(nil)).Elem(),
		value:    reflect.ValueOf(pointer).Elem(),
	}
}

// streamAdapterPackageValueCensus is every NAMED package-level VALUE of package sdk's production
// files that this gate's derivation cannot rule out as an error -- every variable, and every
// constant whose declared type the syntax tree cannot prove methodless. It is hand-written because
// it cannot be anything else: Go has no reflection over a package's variables or constants, so a
// NAME can only become a TYPE by being written down once. The gate below holds it against the
// syntax tree in both directions so that "hand-written" does not mean "stale".
//
// A value declared in a file THIS BUILD DOES NOT COMPILE cannot appear here at all -- naming it
// would not compile -- so it is not demanded here: streamAdapterPackageValuePositions asks
// go/build whether each file is in this build and lists the rest as out of scope, and the
// platform's own fragment, streamAdapterPlatformValueCensus, carries the ones that are.
var streamAdapterPackageValueCensus = map[string]streamAdapterPackageVar{
	"blockActionEvictInterval":                 streamAdapterPackageConstOf(blockActionEvictInterval),
	"contractEjectWindow":                      streamAdapterPackageConstOf(contractEjectWindow),
	"defaultAccountCheckTimeout":               streamAdapterPackageConstOf(defaultAccountCheckTimeout),
	"defaultBlockActionWindowDuration":         streamAdapterPackageConstOf(defaultBlockActionWindowDuration),
	"defaultNetworkCheckTimeout":               streamAdapterPackageConstOf(defaultNetworkCheckTimeout),
	"defaultProviderStatusPollInterval":        streamAdapterPackageConstOf(defaultProviderStatusPollInterval),
	"defaultThroughputSampleInterval":          streamAdapterPackageConstOf(defaultThroughputSampleInterval),
	"defaultThroughputWindowDuration":          streamAdapterPackageConstOf(defaultThroughputWindowDuration),
	"DefaultTunnelDnsAddressIpv6":              streamAdapterPackageConstOf(DefaultTunnelDnsAddressIpv6),
	"dohServerScoresStaleAfter":                streamAdapterPackageConstOf(dohServerScoresStaleAfter),
	"platformTransportMigrateConnectTimeout":   streamAdapterPackageConstOf(platformTransportMigrateConnectTimeout),
	"platformTransportMigrateMaxScheduleDelay": streamAdapterPackageConstOf(platformTransportMigrateMaxScheduleDelay),
	"probeShutdownTimeout":                     streamAdapterPackageConstOf(probeShutdownTimeout),
	"securityPolicyMonitorInterval":            streamAdapterPackageConstOf(securityPolicyMonitorInterval),
	"streamRowIdentityLen":                     streamAdapterPackageConstOf(streamRowIdentityLen),
	"streamRowNameLen":                         streamAdapterPackageConstOf(streamRowNameLen),
	"messageTransportDefaultTimeout":           streamAdapterPackageConstOf(messageTransportDefaultTimeout),
	"windowIdentitiesStaleAfter":               streamAdapterPackageConstOf(windowIdentitiesStaleAfter),
	"base58BigRadix":                           streamAdapterPackageVarOf(&base58BigRadix),
	"base58BigZero":                            streamAdapterPackageVarOf(&base58BigZero),
	"base58Table":                              streamAdapterPackageVarOf(&base58Table),
	"controlDohSettingsConfigure":              streamAdapterPackageVarOf(&controlDohSettingsConfigure),
	"countryCodeColorHexes":                    streamAdapterPackageVarOf(&countryCodeColorHexes),
	"defaultTunnelDnsServersIpv4":              streamAdapterPackageVarOf(&defaultTunnelDnsServersIpv4),
	"defaultTunnelDnsServersIpv6":              streamAdapterPackageVarOf(&defaultTunnelDnsServersIpv6),
	"deviceRpcDefaultAddress":                  streamAdapterPackageVarOf(&deviceRpcDefaultAddress),
	"errStreamAppendInterrupted":               streamAdapterPackageVarOf(&errStreamAppendInterrupted),
	"errStreamInjectedFlushFailure":            streamAdapterPackageVarOf(&errStreamInjectedFlushFailure),
	"ErrStreamKeySpace":                        streamAdapterPackageVarOf(&ErrStreamKeySpace),
	"ErrStreamKeyWidth":                        streamAdapterPackageVarOf(&ErrStreamKeyWidth),
	"ErrStreamStoreConsumed":                   streamAdapterPackageVarOf(&ErrStreamStoreConsumed),
	"ErrStreamStoreLocked":                     streamAdapterPackageVarOf(&ErrStreamStoreLocked),
	"ErrStreamStoreRewound":                    streamAdapterPackageVarOf(&ErrStreamStoreRewound),
	"ErrStreamStoreState":                      streamAdapterPackageVarOf(&ErrStreamStoreState),
	"errMessageFragmentAborted":                streamAdapterPackageVarOf(&errMessageFragmentAborted),
	"errMessageTransportNoCapabilityField":     streamAdapterPackageVarOf(&errMessageTransportNoCapabilityField),
	"errMessageTransportNoHello":               streamAdapterPackageVarOf(&errMessageTransportNoHello),
	"errMessageTransportOverCapability":        streamAdapterPackageVarOf(&errMessageTransportOverCapability),
	"errMessageTransportWrongArm":              streamAdapterPackageVarOf(&errMessageTransportWrongArm),
	"messageCapabilityBounds":                  streamAdapterPackageVarOf(&messageCapabilityBounds),
	"messageFragmentAborts":                    streamAdapterPackageVarOf(&messageFragmentAborts),
	"errMessageTransportMiscorrelated":         streamAdapterPackageVarOf(&errMessageTransportMiscorrelated),
	"errMessageTransportNoArm":                 streamAdapterPackageVarOf(&errMessageTransportNoArm),
	"errMessageTransportNoClient":              streamAdapterPackageVarOf(&errMessageTransportNoClient),
	"errMessageTransportNoServer":              streamAdapterPackageVarOf(&errMessageTransportNoServer),
	"errMessageTransportRefused":               streamAdapterPackageVarOf(&errMessageTransportRefused),
	"errMessageTransportTimeout":               streamAdapterPackageVarOf(&errMessageTransportTimeout),
	"ErrMessageClientNoJwt":                    streamAdapterPackageVarOf(&ErrMessageClientNoJwt),
	"ErrMessageClientNoClientId":               streamAdapterPackageVarOf(&ErrMessageClientNoClientId),
	"ErrMessageClientNoHost":                   streamAdapterPackageVarOf(&ErrMessageClientNoHost),
	"multiPartPublicSuffixes":                  streamAdapterPackageVarOf(&multiPartPublicSuffixes),
	"probeDnsTargets":                          streamAdapterPackageVarOf(&probeDnsTargets),
	"probeHttpTargets":                         streamAdapterPackageVarOf(&probeHttpTargets),
	"publicIdentityKeyHashEncoding":            streamAdapterPackageVarOf(&publicIdentityKeyHashEncoding),
	"streamStoreHeldHere":                      streamAdapterPackageVarOf(&streamStoreHeldHere),
	"streamStoreHeldHereMutex":                 streamAdapterPackageVarOf(&streamStoreHeldHereMutex),
	"streamStoreSentinelRulings":               streamAdapterPackageVarOf(&streamStoreSentinelRulings),

	// message_route.go, the route to the server's own endpoint (ledger 268). Added 2026-10-03:
	// the census had not been told about them, and this gate had not been run since they landed.
	"ErrMessageRouteBadEndpoint":   streamAdapterPackageVarOf(&ErrMessageRouteBadEndpoint),
	"ErrMessageRouteBadMode":       streamAdapterPackageVarOf(&ErrMessageRouteBadMode),
	"ErrMessageRouteBadPin":        streamAdapterPackageVarOf(&ErrMessageRouteBadPin),
	"ErrMessageRouteNoEndpoint":    streamAdapterPackageVarOf(&ErrMessageRouteNoEndpoint),
	"ErrMessageRoutePinMismatch":   streamAdapterPackageVarOf(&ErrMessageRoutePinMismatch),
	"errMessageRouteClosed":        streamAdapterPackageVarOf(&errMessageRouteClosed),
	"messageRouteDialTimeout":      streamAdapterPackageConstOf(messageRouteDialTimeout),
	"messageRouteHandshakeTimeout": streamAdapterPackageConstOf(messageRouteHandshakeTimeout),
	"messageRouteMaxBackoff":       streamAdapterPackageConstOf(messageRouteMaxBackoff),
	"messageRouteMinBackoff":       streamAdapterPackageConstOf(messageRouteMinBackoff),
	"messageRoutePingInterval":     streamAdapterPackageConstOf(messageRoutePingInterval),
	"messageRouteWriteTimeout":     streamAdapterPackageConstOf(messageRouteWriteTimeout),

	// UPSTREAM sdk's, with the merge of urnetwork/sdk main (msgrepo ledger 277). The census is
	// over the whole package by design, so a value upstream declares is named here like any other.
	"BlockActionReasonBlocker":              streamAdapterPackageConstOf(BlockActionReasonBlocker),
	"BlockActionReasonOverride":             streamAdapterPackageConstOf(BlockActionReasonOverride),
	"BlockActionReasonSecurity":             streamAdapterPackageConstOf(BlockActionReasonSecurity),
	"BlockActionReasonSecurityBittorrent":   streamAdapterPackageConstOf(BlockActionReasonSecurityBittorrent),
	"BlockActionReasonSecurityEncrypted":    streamAdapterPackageConstOf(BlockActionReasonSecurityEncrypted),
	"BlockActionReasonSecurityIp":           streamAdapterPackageConstOf(BlockActionReasonSecurityIp),
	"BlockActionReasonSecurityPort":         streamAdapterPackageConstOf(BlockActionReasonSecurityPort),
	"BlockActionReasonSecuritySmtp":         streamAdapterPackageConstOf(BlockActionReasonSecuritySmtp),
	"EmojiTagMaxCount":                      streamAdapterPackageConstOf(EmojiTagMaxCount),
	"EmojiTagSuggestMaxCount":               streamAdapterPackageConstOf(EmojiTagSuggestMaxCount),
	"ErrWalletCircleTransferRequestId":      streamAdapterPackageVarOf(&ErrWalletCircleTransferRequestId),
	"ExtenderGossipStateConnected":          streamAdapterPackageConstOf(ExtenderGossipStateConnected),
	"ExtenderGossipStateConnecting":         streamAdapterPackageConstOf(ExtenderGossipStateConnecting),
	"ExtenderGossipStateDisconnected":       streamAdapterPackageConstOf(ExtenderGossipStateDisconnected),
	"Version":                               streamAdapterPackageVarOf(&Version),
	"bundledExtenderRootPublicKeyHexes":     streamAdapterPackageVarOf(&bundledExtenderRootPublicKeyHexes),
	"clientEventFlushInterval":              streamAdapterPackageConstOf(clientEventFlushInterval),
	"clientEventPropKeys":                   streamAdapterPackageVarOf(&clientEventPropKeys),
	"currentLogDir":                         streamAdapterPackageVarOf(&currentLogDir),
	"currentLogDirMu":                       streamAdapterPackageVarOf(&currentLogDirMu),
	"currentLogRoot":                        streamAdapterPackageVarOf(&currentLogRoot),
	"defaultMobileTrimLevelReporter":        streamAdapterPackageVarOf(&defaultMobileTrimLevelReporter),
	"defaultSubprotocolsQueryTimeout":       streamAdapterPackageConstOf(defaultSubprotocolsQueryTimeout),
	"deviceRpcH1PlusDisabled":               streamAdapterPackageVarOf(&deviceRpcH1PlusDisabled),
	"errApiRequestFailed":                   streamAdapterPackageVarOf(&errApiRequestFailed),
	"errApiRequestReturnedWithoutCallback":  streamAdapterPackageVarOf(&errApiRequestReturnedWithoutCallback),
	"errByJwtNoClientId":                    streamAdapterPackageVarOf(&errByJwtNoClientId),
	"errDeviceRemoteLocationInvalid":        streamAdapterPackageVarOf(&errDeviceRemoteLocationInvalid),
	"errDeviceRemoteLocationReadFailed":     streamAdapterPackageVarOf(&errDeviceRemoteLocationReadFailed),
	"errDeviceRemoteLocationSuperseded":     streamAdapterPackageVarOf(&errDeviceRemoteLocationSuperseded),
	"errDeviceRemoteLocationUnavailable":    streamAdapterPackageVarOf(&errDeviceRemoteLocationUnavailable),
	"errFindRoutesRemoved":                  streamAdapterPackageVarOf(&errFindRoutesRemoved),
	"errGuestUpgradeRouteRemoved":           streamAdapterPackageVarOf(&errGuestUpgradeRouteRemoved),
	"errLocationsResultInvalid":             streamAdapterPackageVarOf(&errLocationsResultInvalid),
	"errPeerPinStoreBudget":                 streamAdapterPackageVarOf(&errPeerPinStoreBudget),
	"errPeerPinStoreCapacity":               streamAdapterPackageVarOf(&errPeerPinStoreCapacity),
	"errPeerPinStoreClosed":                 streamAdapterPackageVarOf(&errPeerPinStoreClosed),
	"errPeerPinStoreCorrupt":                streamAdapterPackageVarOf(&errPeerPinStoreCorrupt),
	"errPeerPinStoreIO":                     streamAdapterPackageVarOf(&errPeerPinStoreIO),
	"errPeerPinStoreOversize":               streamAdapterPackageVarOf(&errPeerPinStoreOversize),
	"errPeerPinStoreRollback":               streamAdapterPackageVarOf(&errPeerPinStoreRollback),
	"errPeerPinStoreSuperseded":             streamAdapterPackageVarOf(&errPeerPinStoreSuperseded),
	"errTransferDiagnosticSnapshotTooLarge": streamAdapterPackageVarOf(&errTransferDiagnosticSnapshotTooLarge),
	"errTransferDiagnosticUnavailable":      streamAdapterPackageVarOf(&errTransferDiagnosticUnavailable),
	"errUploadLogsRpcClosed":                streamAdapterPackageVarOf(&errUploadLogsRpcClosed),
	"errUploadLogsUnreported":               streamAdapterPackageVarOf(&errUploadLogsUnreported),
	"extenderNetworkClientConfigure":        streamAdapterPackageVarOf(&extenderNetworkClientConfigure),
	"extenderNetworkClientEnabled":          streamAdapterPackageVarOf(&extenderNetworkClientEnabled),
	"extenderNodeEnabled":                   streamAdapterPackageVarOf(&extenderNodeEnabled),
	"extenderProvideRoleEnabled":            streamAdapterPackageVarOf(&extenderProvideRoleEnabled),
	"extenderProvideStatusEpoch":            streamAdapterPackageConstOf(extenderProvideStatusEpoch),
	"extenderStatusEpoch":                   streamAdapterPackageConstOf(extenderStatusEpoch),
	"extenderStoreReadOnly":                 streamAdapterPackageVarOf(&extenderStoreReadOnly),
	"licenseJSON":                           streamAdapterPackageVarOf(&licenseJSON),
	"loadLicenseFile":                       streamAdapterPackageVarOf(&loadLicenseFile),
	"localPreferenceCatalog":                streamAdapterPackageVarOf(&localPreferenceCatalog),
	"logSeverities":                         streamAdapterPackageVarOf(&logSeverities),
	"logVerbosityMu":                        streamAdapterPackageVarOf(&logVerbosityMu),
	"memoryClassNames":                      streamAdapterPackageVarOf(&memoryClassNames),
	"minRefreshTimeout":                     streamAdapterPackageConstOf(minRefreshTimeout),
	"mobileH1ReceiveAckHandoffWaitTimeout":  streamAdapterPackageConstOf(mobileH1ReceiveAckHandoffWaitTimeout),
	"mobileH1ReceivePackHandoffWaitTimeout": streamAdapterPackageConstOf(mobileH1ReceivePackHandoffWaitTimeout),
	"mobileIdleMemoryActivity":              streamAdapterPackageVarOf(&mobileIdleMemoryActivity),
	"mobileIdleMemoryTrimAfter":             streamAdapterPackageVarOf(&mobileIdleMemoryTrimAfter),
	"mobileIdleMemoryTrimBefore":            streamAdapterPackageVarOf(&mobileIdleMemoryTrimBefore),
	"mobileIdleMemoryTrimBelow":             streamAdapterPackageVarOf(&mobileIdleMemoryTrimBelow),
	"mobileIdleMemoryTrimCooldown":          streamAdapterPackageConstOf(mobileIdleMemoryTrimCooldown),
	"mobileIdleMemoryTrimCooldowns":         streamAdapterPackageVarOf(&mobileIdleMemoryTrimCooldowns),
	"mobileIdleMemoryTrimCount":             streamAdapterPackageVarOf(&mobileIdleMemoryTrimCount),
	"mobileIdleMemoryTrimDeferred":          streamAdapterPackageVarOf(&mobileIdleMemoryTrimDeferred),
	"mobileIdleMemoryTrimDelay":             streamAdapterPackageConstOf(mobileIdleMemoryTrimDelay),
	"mobileIdleMemoryTrimDropped":           streamAdapterPackageVarOf(&mobileIdleMemoryTrimDropped),
	"mobileIdleMemoryTrimRetryDelay":        streamAdapterPackageConstOf(mobileIdleMemoryTrimRetryDelay),
	"mobileIdleMemoryTrimmerOnce":           streamAdapterPackageVarOf(&mobileIdleMemoryTrimmerOnce),
	"mobileIdleMemoryTrimmerStarted":        streamAdapterPackageVarOf(&mobileIdleMemoryTrimmerStarted),
	"mobileMemorySampleInterval":            streamAdapterPackageConstOf(mobileMemorySampleInterval),
	"mobilePhysicalFootprintCurrent":        streamAdapterPackageVarOf(&mobilePhysicalFootprintCurrent),
	"mobilePhysicalFootprintPeak":           streamAdapterPackageVarOf(&mobilePhysicalFootprintPeak),
	"mobilePhysicalPressureArmed":           streamAdapterPackageVarOf(&mobilePhysicalPressureArmed),
	"mobilePhysicalPressureByteCount":       streamAdapterPackageVarOf(&mobilePhysicalPressureByteCount),
	"mobilePhysicalPressureCount":           streamAdapterPackageVarOf(&mobilePhysicalPressureCount),
	"mobileRuntimePressureArmed":            streamAdapterPackageVarOf(&mobileRuntimePressureArmed),
	"mobileTcpSequenceIdleTimeout":          streamAdapterPackageConstOf(mobileTcpSequenceIdleTimeout),
	"mobileTrimLevelActionCount":            streamAdapterPackageVarOf(&mobileTrimLevelActionCount),
	"mobileTrimLevelCount":                  streamAdapterPackageVarOf(&mobileTrimLevelCount),
	"mobileTrimLevelDropped":                streamAdapterPackageVarOf(&mobileTrimLevelDropped),
	"mobileTrimLevelLast":                   streamAdapterPackageVarOf(&mobileTrimLevelLast),
	"noExpirationRefreshTimeout":            streamAdapterPackageConstOf(noExpirationRefreshTimeout),
	"pointsLeaderboardTestBeforeFetch":      streamAdapterPackageVarOf(&pointsLeaderboardTestBeforeFetch),
	"pointsLeaderboardTierPercents":         streamAdapterPackageVarOf(&pointsLeaderboardTierPercents),
	"providerPriorsStaleAfter":              streamAdapterPackageConstOf(providerPriorsStaleAfter),
	"purchaseReportBackoffMillis":           streamAdapterPackageVarOf(&purchaseReportBackoffMillis),
	"redactAddrBytesPattern":                streamAdapterPackageVarOf(&redactAddrBytesPattern),
	"redactIPv4Pattern":                     streamAdapterPackageVarOf(&redactIPv4Pattern),
	"redactIPv6Pattern":                     streamAdapterPackageVarOf(&redactIPv6Pattern),
	"redactUUIDPattern":                     streamAdapterPackageVarOf(&redactUUIDPattern),
	"runtimeTotalMetricLock":                streamAdapterPackageVarOf(&runtimeTotalMetricLock),
	"runtimeTotalMetricSamples":             streamAdapterPackageVarOf(&runtimeTotalMetricSamples),
	"snArtifactFetchTimeout":                streamAdapterPackageConstOf(snArtifactFetchTimeout),
	"snClaimReceiptPoll":                    streamAdapterPackageConstOf(snClaimReceiptPoll),
	"snClaimReceiptTimeout":                 streamAdapterPackageConstOf(snClaimReceiptTimeout),
	"snClaimTimeout":                        streamAdapterPackageConstOf(snClaimTimeout),
	"snClaimsTimeout":                       streamAdapterPackageConstOf(snClaimsTimeout),
	"snHexRe":                               streamAdapterPackageVarOf(&snHexRe),
	"snRpcTimeout":                          streamAdapterPackageConstOf(snRpcTimeout),
	"snWeiPerTao":                           streamAdapterPackageVarOf(&snWeiPerTao),
	"subscriptionStoreAppleRe":              streamAdapterPackageVarOf(&subscriptionStoreAppleRe),
	"subscriptionStoreGoogleRe":             streamAdapterPackageVarOf(&subscriptionStoreGoogleRe),
	"subscriptionStoreStripeRe":             streamAdapterPackageVarOf(&subscriptionStoreStripeRe),
	"testingBeforeExtenderStatusWatch":      streamAdapterPackageVarOf(&testingBeforeExtenderStatusWatch),
	"transferDiagLogSeconds":                streamAdapterPackageVarOf(&transferDiagLogSeconds),
	"transferDiagnosticSnapshotsEnabled":    streamAdapterPackageVarOf(&transferDiagnosticSnapshotsEnabled),
	"tunnelLocalIpv6Prefix":                 streamAdapterPackageVarOf(&tunnelLocalIpv6Prefix),
}

// streamAdapterPackageConstOf answers a package-level CONSTANT's DECLARED type, the same way
// streamAdapterPackageVarOf answers a variable's. It takes the value rather than its address,
// because a constant has no address, and the type parameter is what carries the declaration's
// type: for `const e errString = "x"` T is inferred as errString, and errString's method set --
// not the spelling of the initialiser -- is what decides whether it is an error.
func streamAdapterPackageConstOf[T any](value T) streamAdapterPackageVar {
	return streamAdapterPackageVar{
		declared: reflect.TypeOf((*T)(nil)).Elem(),
		value:    reflect.ValueOf(value),
	}
}

// streamAdapterCensus merges the portable census with this platform's fragment and the native
// extender's fragment (message_stream_adapter_census_extender_*_test.go). The fragment
// exists because a package-level value declared in a build-constrained file can only be NAMED by
// source this build compiles: streamAdapterPlatformValueCensus lives beside the production
// exclusion files' own constraints, and the scope check below is go/build's answer rather than a
// reading of a comment.
func streamAdapterCensus() map[string]streamAdapterPackageVar {
	merged := map[string]streamAdapterPackageVar{}
	maps.Copy(merged, streamAdapterPackageValueCensus)
	maps.Copy(merged, streamAdapterPlatformValueCensus)
	maps.Copy(merged, streamAdapterExtenderNativeValueCensus)
	return merged
}

// streamAdapterNonSentinels merges the portable and platform halves of the ruling table for
// package-level error values that are NOT the store's sentinels.
func streamAdapterNonSentinels() map[string]string {
	merged := map[string]string{}
	maps.Copy(merged, streamAdapterNonSentinelRulings)
	maps.Copy(merged, streamAdapterPlatformNonSentinelRulings)
	return merged
}

// streamAdapterNonSentinelRulings is the second half of the class's totality, and it is here
// rather than in production for one reason: a value ruled here is NOT something the adapter maps,
// so putting it in streamStoreSentinelRulings would be claiming a verdict for a refusal
// SenderRatchet.Next can never meet.
//
// EVERY ENTRY IS CHECKED, not taken on trust. The gate holds that a value ruled here is never
// handed to fmt.Errorf anywhere in production sdk -- which is the only way this package puts a
// value into an error chain, and therefore the only way one can reach classify. Wrap one with %w
// tomorrow and this stops being a valid excuse and the gate says so.
var streamAdapterNonSentinelRulings = map[string]string{
	"errMessageTransportNoClient":          "the message-server binding's refusal that its config named no connect client. It is raised by newMessageTransport, which the reserver's call graph does not reach; a store failure cannot carry it because the store never constructs a transport",
	"errMessageTransportNoServer":          "the same construction refusal for a config that named no server client_id. Same seat, same reason",
	"errMessageTransportNoArm":             "the binding's refusal that a request body is not an arm of §4.3's body oneof. It is decided off the compiled descriptor inside the transport's own send path and reaches no store call",
	"errMessageTransportRefused":           "the binding's refusal that connect would not take the frame. It is raised on the transport's send path, which the reserver cannot reach: a reserver talks to a StreamStore and a StreamStore opens files",
	"errMessageTransportMiscorrelated":     "the binding's refusal that a response arrived under another request's request_id. It is raised inside messageTransport.Call and nothing in the store's chain can carry it",
	"errMessageTransportNoHello":           "the binding's local refusal that a request needing this connection's server_nonce cannot precede Hello. It is decided in messageTransport.Call off the compiled descriptor, before a waiter is registered and before anything reaches a wire; a reserver talks to a StreamStore and a StreamStore has no connection",
	"errMessageTransportOverCapability":    "the binding's local refusal that a request exceeds a bound the server advertised in §4.3.1 Capabilities. Raised in messageTransport.Call over a proto.Size of the request; no store call can produce it and no store failure can carry it",
	"errMessageTransportNoCapabilityField": "the binding's refusal that protocol.Capabilities declares no field a bound names. It is a descriptor lookup inside the transport and reaches no store",
	"errMessageTransportWrongArm":          "the binding's refusal to read a REASON_OK answer carried on a body arm that is not the one the request travelled in. Raised in messageTransport.Hello over a decoded response; a StreamStore answers no arms",
	"errMessageFragmentAborted":            "the binding's typed §4.6 abort, raised inside the receive callback when a fragment is not the one the reassembly was waiting for. It is raised over a protocol.MessageServerFragment that connect delivered and reaches no store call at all; a reserver talks to a StreamStore and a StreamStore has no fragments",
	"errMessageTransportTimeout":           "the binding's typed timeout, raised when no response carrying a request_id arrived before the deadline. It is a TRANSPORT deadline and not a store refusal; SenderRatchet.Next never meets it, because the reserver's error chain comes from a StreamStore and a StreamStore has no deadline",
	"ErrMessageClientNoJwt":                "NewMessageClient's refusal that no by_client_jwt was handed in. It is raised by a free function that CONSTRUCTS a platform-attached connect client, before any store exists; the reserver's call graph reaches a StreamStore and a StreamStore mints no credentials",
	"ErrMessageClientNoClientId":           "NewMessageClient's refusal that the credential names no client_id, so the client it would build is at the zero id. Same seat as the one above: a construction refusal taken before a client exists, and no method the adapter holds can produce it",
	"ErrMessageClientNoHost":               "NewMessageClient's refusal that the config named neither a host nor both absolute service urls, so there is nowhere to dial. It is decided over the config alone, in a free function no store call reaches",
	// message_route.go, the route to the server's own endpoint (ledger 268). Not one of the stream
	// store's refusals: the route client is a transport, and the adapter holds no route client.
	"ErrMessageRouteBadEndpoint": "the route client's refusal that the configured endpoint is not a wss:// url with a host. It is decided when a MessageRouteClient is built, before any stream exists",
	"ErrMessageRouteBadMode":     "the route client's refusal of a route mode it does not know. Same seat: construction",
	"ErrMessageRouteBadPin":      "the route client's refusal of a pin that is not 64 hex characters of SHA-256. Same seat",
	"ErrMessageRouteNoEndpoint":  "the route client's refusal that a route to the server's own endpoint named none. Same seat",
	"ErrMessageRoutePinMismatch": "the route client's TLS refusal that the server presented a key other than the pinned one. It ends a dial, on the route client's side of the transport",
	"errMessageRouteClosed":      "the route client's refusal to carry a frame after Close, on its own send path",

	// UPSTREAM sdk's, with the merge of urnetwork/sdk main (msgrepo ledger 277). Each is the refusal of
	// a part of package sdk the stream store has no part in, and each is measured below like any other.
	"ErrWalletCircleTransferRequestId":      "the wallet API's refusal of a Circle transfer with no persisted request_id",
	"errApiRequestFailed":                   "the API client's failure of one request",
	"errApiRequestReturnedWithoutCallback":  "the API client's failure that a request returned without calling its callback",
	"errByJwtNoClientId":                    "the local device's refusal of a by_client_jwt with no client_id claim",
	"errDeviceRemoteLocationInvalid":        "DeviceRemote's refusal of a malformed current-location answer",
	"errDeviceRemoteLocationReadFailed":     "DeviceRemote's failure to read the current location",
	"errDeviceRemoteLocationSuperseded":     "DeviceRemote's refusal of a current location read across a device change",
	"errDeviceRemoteLocationUnavailable":    "DeviceRemote's answer that no current location is available",
	"errFindRoutesRemoved":                  "the API client's refusal to call a route the server removed",
	"errGuestUpgradeRouteRemoved":           "the API client's refusal to call the removed guest-upgrade route",
	"errLocationsResultInvalid":             "the API client's refusal of a malformed provider-locations answer",
	"errPeerPinStoreBudget":                 "the bounded peer key-pin store's refusal that its memory budget is full",
	"errPeerPinStoreCapacity":               "the bounded peer key-pin store's refusal that its capacity is full",
	"errPeerPinStoreClosed":                 "the bounded peer key-pin store's refusal after close",
	"errPeerPinStoreCorrupt":                "the bounded peer key-pin store's refusal of an invalid persisted store",
	"errPeerPinStoreIO":                     "the bounded peer key-pin store's persistence failure; it wraps this one, inside the store's own commit",
	"errPeerPinStoreOversize":               "the bounded peer key-pin store's refusal of a persisted store over 128 KiB",
	"errPeerPinStoreRollback":               "the bounded peer key-pin store's refusal of a generation rollback or conflict",
	"errPeerPinStoreSuperseded":             "the bounded peer key-pin store's refusal from a superseded owner",
	"errTransferDiagnosticSnapshotTooLarge": "the transfer diagnostics' refusal of a snapshot over 64 KiB",
	"errTransferDiagnosticUnavailable":      "the transfer diagnostics' refusal on a device that has not opted in",
	"errUploadLogsRpcClosed":                "DeviceRemote's report to an UploadLogs callback that the device rpc closed before the upload reported its result",
	"errUploadLogsUnreported":               "DeviceRemote's report to an UploadLogs callback that the device process does not report the upload's result",
}

// streamAdapterPredeclaredTypeNames is the set of type names a CONSTANT's declared type can be
// without being a defined type with a method set. The Go spec's constant types are boolean,
// rune, integer, floating-point, complex and string; none of the predeclared spellings of those
// has methods, so a constant declared with one cannot implement error.
var streamAdapterPredeclaredTypeNames = map[string]bool{
	"bool": true, "string": true, "int": true, "int8": true, "int16": true, "int32": true,
	"int64": true, "uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "byte": true, "rune": true, "float32": true, "float64": true,
	"complex64": true, "complex128": true,
}

// streamAdapterValueDecl is one package-level value declaration of package sdk, as the syntax
// tree has it. spec is the EFFECTIVE ValueSpec: inside a const group a spec with neither a type
// nor a value repeats the preceding one, and the type it inherits is that one's.
type streamAdapterValueDecl struct {
	file     string
	constant bool
	spec     *ast.ValueSpec
	position string
}

// streamAdapterLocalTypes answers every type package sdk's production files DECLARE, and the
// subset of them carrying an Error method. Both are read off the syntax tree, so "which of this
// package's types is an error type" is derived and not listed.
func streamAdapterLocalTypes(parsed map[string]*ast.File) (map[string]bool, map[string]bool) {
	declared := map[string]bool{}
	withError := map[string]bool{}
	for _, file := range parsed {
		for _, declaration := range file.Decls {
			if general, ok := declaration.(*ast.GenDecl); ok && general.Tok == token.TYPE {
				for _, spec := range general.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok {
						declared[typeSpec.Name.Name] = true
					}
				}
				continue
			}
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || len(function.Recv.List) == 0 || function.Name.Name != "Error" {
				continue
			}
			receiver := function.Recv.List[0].Type
			if star, ok := receiver.(*ast.StarExpr); ok {
				receiver = star.X
			}
			if identifier, ok := receiver.(*ast.Ident); ok {
				withError[identifier.Name] = true
			}
		}
	}
	return declared, withError
}

// streamAdapterConstantNeedsCensus is THE DERIVATION over a package-level constant: can it be an
// error at all, as far as the syntax tree can tell?
//
// A constant's type is a basic type or a DEFINED type whose underlying type is basic, and for it
// to implement error that defined type must carry an Error method. So:
//
//	an explicit or inherited type that is PREDECLARED            -> cannot be an error
//	an explicit or inherited type that package sdk declares and
//	  which has no Error method                                  -> cannot be an error
//	an explicit or inherited type that package sdk declares and
//	  which HAS one                                              -> CENSUS IT
//	no type at all, and every operand is a literal, iota, or
//	  another constant of this package that is itself untyped     -> cannot be an error: an
//	                                                                untyped constant takes one of
//	                                                                Go's default types -- bool,
//	                                                                rune, int, float64,
//	                                                                complex128, string -- and
//	                                                                none of them has methods
//	anything else -- an IMPORTED type, a conversion or an
//	  identifier from another package, a spec shape this walk
//	  does not understand                                        -> CENSUS IT, fail closed
//
// The last line is the whole safety of it. This gate cannot see another package's method set, and
// syscall.Errno -- which two of package sdk's own constants are -- implements error. A constant
// whose type this walk cannot resolve is not assumed harmless; it is made to carry an entry, and
// the entry's type parameter is the compiler's answer rather than this walk's.
func streamAdapterConstantNeedsCensus(
	spec *ast.ValueSpec,
	decls map[string]streamAdapterValueDecl,
	declared map[string]bool,
	withError map[string]bool,
	seen map[string]bool,
) (bool, string) {
	if spec.Type != nil {
		switch typed := spec.Type.(type) {
		case *ast.Ident:
			switch {
			case streamAdapterPredeclaredTypeNames[typed.Name]:
				return false, "declared with the predeclared type " + typed.Name + ", which has no methods"
			case withError[typed.Name]:
				return true, "declared with " + typed.Name + ", a type package sdk declares WITH an Error method"
			case declared[typed.Name]:
				return false, "declared with " + typed.Name + ", a type package sdk declares and which has no Error method"
			}
			return true, "declared with the type " + typed.Name + ", which this gate cannot resolve"
		case *ast.SelectorExpr:
			return true, "declared with the IMPORTED type " + streamAdapterTypeText(typed) + ", whose method set this gate cannot see"
		}
		return true, fmt.Sprintf("declared with a type expression this gate does not understand (%T)", spec.Type)
	}
	if len(spec.Values) == 0 {
		return true, "neither a type nor a value this gate could follow"
	}
	for _, value := range spec.Values {
		if needs, why := streamAdapterConstantExprNeedsCensus(value, decls, declared, withError, seen); needs {
			return true, why
		}
	}
	return false, "untyped: every operand is a literal, iota, or another untyped constant of this package"
}

func streamAdapterConstantExprNeedsCensus(
	expression ast.Expr,
	decls map[string]streamAdapterValueDecl,
	declared map[string]bool,
	withError map[string]bool,
	seen map[string]bool,
) (bool, string) {
	switch typed := expression.(type) {
	case *ast.BasicLit:
		return false, ""
	case *ast.ParenExpr:
		return streamAdapterConstantExprNeedsCensus(typed.X, decls, declared, withError, seen)
	case *ast.UnaryExpr:
		return streamAdapterConstantExprNeedsCensus(typed.X, decls, declared, withError, seen)
	case *ast.BinaryExpr:
		if needs, why := streamAdapterConstantExprNeedsCensus(typed.X, decls, declared, withError, seen); needs {
			return true, why
		}
		return streamAdapterConstantExprNeedsCensus(typed.Y, decls, declared, withError, seen)
	case *ast.Ident:
		if typed.Name == "iota" || typed.Name == "true" || typed.Name == "false" {
			return false, ""
		}
		other, ok := decls[typed.Name]
		if !ok || !other.constant {
			return true, "initialised from " + typed.Name + ", which this gate cannot resolve to a constant of this package"
		}
		if seen[typed.Name] {
			return true, "a constant reference cycle this gate will not follow"
		}
		seen[typed.Name] = true
		return streamAdapterConstantNeedsCensus(other.spec, decls, declared, withError, seen)
	case *ast.CallExpr:
		switch fun := typed.Fun.(type) {
		case *ast.Ident:
			switch {
			case streamAdapterPredeclaredTypeNames[fun.Name]:
				return false, ""
			case withError[fun.Name]:
				return true, "converted to " + fun.Name + ", a type package sdk declares WITH an Error method"
			case declared[fun.Name]:
				return false, ""
			}
			return true, "converted by " + fun.Name + ", which this gate cannot resolve"
		case *ast.SelectorExpr:
			return true, "converted to the IMPORTED type " + streamAdapterTypeText(fun) + ", whose method set this gate cannot see"
		}
		return true, "a call this gate does not understand"
	case *ast.SelectorExpr:
		return true, "initialised from the IMPORTED identifier " + streamAdapterTypeText(typed) + ", whose type this gate cannot see"
	}
	return true, fmt.Sprintf("an expression this gate does not understand (%T)", expression)
}

// streamAdapterBuiltUnder is go/build's OWN answer to each production file's build constraints,
// under a context the caller chooses: does that build compile it? A file a build does not compile
// declares names no source in that build can spell, so no census entry for one could exist there
// -- and a gate that demanded one would be a gate that cannot pass on that platform.
//
// IT TAKES THE CONTEXT RATHER THAN READING build.Default, and that is not generality for its own
// sake. On windows/amd64 the scope narrowing removes no NAME at all: every value this enumeration
// classifies sits in a file this build compiles, so the clause would be inert here and defended by
// nothing. Handing it a context lets TestTheValueCensusScopeIsThisBuildsOwnFileSet run the same
// enumeration under GOOS=linux, where the two syscall.Errno constants of the Windows exclusion file
// DO fall out of reach, and watch them move from the demanded set into the out-of-scope list. The
// platform this clause exists for is reachable from the platform this suite runs on.
func streamAdapterBuiltUnder(t *testing.T, parsed map[string]*ast.File, context build.Context) map[string]bool {
	t.Helper()
	built := map[string]bool{}
	compiled := 0
	for name := range parsed {
		match, err := context.MatchFile(".", name)
		if err != nil {
			t.Fatalf("go/build could not answer whether a %s build compiles %s: %v", context.GOOS, name, err)
		}
		built[name] = match
		if match {
			compiled += 1
		}
	}
	if compiled == 0 {
		t.Fatalf("go/build says a %s build compiles none of package sdk's production files, so the census scope is empty", context.GOOS)
	}
	return built
}

// streamAdapterPackageValuePositions reads every package-level VALUE of package sdk's production
// files off the syntax tree -- VAR and CONST alike -- and answers name -> position for the ones
// the census has to hold, plus the complement, plus the names this platform's build puts out of
// reach.
//
// THE CONST HALF IS NEW, AND IT IS WHY. The enumeration used to walk `general.Tok != token.VAR`
// and skip everything else, so a sentinel spelled as a CONSTANT of a named string type with an
// Error method was never enumerated, never censused, never ruled, and forwarded by classify as
// TRANSIENT. Worse than missing it: adding such a constant to the census made the gate report it
// as a name "package sdk no longer declares", so the failure message told the reader to DELETE the
// entry -- and the gate went green on exactly the move that hid the sentinel. The enumeration is
// over names off the syntax tree for constants for the same reason it is for variables: a
// declaration must spell the name it declares, whatever shape its initialiser takes.
//
// The BLANK identifier is answered in the complement on a derivation rather than by taste -- a
// blank name cannot be referenced, so no errors.Is can reach it, no ruling could name it and no
// census entry could be written for it.
func streamAdapterPackageValuePositions(t *testing.T) (map[string]string, []string, []string, map[string]string) {
	t.Helper()
	return streamAdapterValuePositionsUnder(t, build.Default)
}

func streamAdapterValuePositionsUnder(t *testing.T, context build.Context) (map[string]string, []string, []string, map[string]string) {
	t.Helper()
	fileSet, parsed, _ := streamAdapterParse(t)
	built := streamAdapterBuiltUnder(t, parsed, context)
	declaredTypes, errorTypes := streamAdapterLocalTypes(parsed)

	decls := map[string]streamAdapterValueDecl{}
	complement := []string{}
	// In sorted order, so the result never depends on map iteration (see the variant rule below).
	for _, name := range slices.Sorted(maps.Keys(parsed)) {
		file := parsed[name]
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || (general.Tok != token.VAR && general.Tok != token.CONST) {
				continue
			}
			var previous *ast.ValueSpec
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				effective := value
				if general.Tok == token.CONST && value.Type == nil && len(value.Values) == 0 && previous != nil {
					effective = previous
				} else {
					previous = value
				}
				for _, identifier := range value.Names {
					position := fileSet.Position(identifier.Pos()).String()
					if identifier.Name == "_" {
						complement = append(complement, fmt.Sprintf(
							"a blank identifier at %s in %s -- unreferenceable, so no errors.Is can reach it", position, name))
						continue
					}
					// A NAME DECLARED IN MORE THAN ONE FILE is a set of build variants: extender_node_js.go
					// and extender_node_native.go both declare extenderNodeEnabled, and exactly one of them is
					// this build's. That one is the declaration this build can name, so it is kept whatever
					// order the files come in. Without this rule the kept declaration was whichever file a map
					// yielded last, and the gate passed or failed by iteration order (fork run 37119743194).
					if existing, seen := decls[identifier.Name]; seen && built[existing.file] && !built[name] {
						continue
					}
					decls[identifier.Name] = streamAdapterValueDecl{
						file:     name,
						constant: general.Tok == token.CONST,
						spec:     effective,
						position: position,
					}
				}
			}
		}
	}

	named := map[string]string{}
	outOfScope := []string{}
	excluded := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(decls)) {
		declaration := decls[name]
		if declaration.constant {
			needs, why := streamAdapterConstantNeedsCensus(
				declaration.spec, decls, declaredTypes, errorTypes, map[string]bool{})
			if !needs {
				complement = append(complement, fmt.Sprintf("the constant %s -- %s", name, why))
				excluded[name] = why
				continue
			}
			if !built[declaration.file] {
				outOfScope = append(outOfScope, fmt.Sprintf(
					"the constant %s in %s (%s) -- this build does not compile that file, so no source here can name it", name, declaration.file, why))
				continue
			}
			named[name] = declaration.position
			continue
		}
		if !built[declaration.file] {
			outOfScope = append(outOfScope, fmt.Sprintf(
				"the variable %s in %s -- this build does not compile that file, so no source here can name it", name, declaration.file))
			continue
		}
		named[name] = declaration.position
	}
	slices.Sort(complement)
	slices.Sort(outOfScope)
	return named, complement, outOfScope, excluded
}

// streamAdapterSentinelDeclarations answers name -> the declared error value, for every
// package-level value of package sdk WHOSE DECLARED TYPE IS AN ERROR, plus the COMPLEMENT the two
// narrowings removed: the constants the syntax tree proves cannot be errors, the blanks, the
// censused values whose declared type does not implement error, and the names this build puts out
// of reach.
//
// It fails the test outright when the census and the syntax tree disagree, because a census that
// has fallen behind the tree is a gate that has stopped reading the package.
func streamAdapterSentinelDeclarations(t *testing.T) (map[string]error, []string) {
	t.Helper()
	named, complement, outOfScope, excluded := streamAdapterPackageValuePositions(t)
	census := streamAdapterCensus()
	if len(named) == 0 {
		t.Fatal("this gate found no named package-level value at all, so it is holding nothing")
	}
	missing := []string{}
	for name, position := range named {
		if _, censused := census[name]; !censused {
			missing = append(missing, fmt.Sprintf("%s (%s)", name, position))
		}
	}
	// A CENSUS ENTRY THE ENUMERATION DID NOT REACH IS ONE OF TWO DIFFERENT THINGS, and the
	// message SAYS WHICH, because the wrong answer to it is how a sentinel gets hidden. An entry
	// for a name this gate's own derivation already excluded is merely REDUNDANT and safe to
	// remove. An entry for a name the enumeration never saw at all is this gate having stopped
	// reading the package, and removing that one is exactly the move that hides the sentinel it
	// was added for. The reason is looked up rather than guessed, so the two can never be
	// reported as one another.
	stale := []string{}
	for name := range census {
		if _, declared := named[name]; declared {
			continue
		}
		if why, ok := excluded[name]; ok {
			stale = append(stale, fmt.Sprintf("%s -- REDUNDANT, and safe to remove: this gate's own derivation already excluded it (%s)", name, why))
			continue
		}
		stale = append(stale, name+" -- NOT ENUMERATED AT ALL, and NOT safe to remove")
	}
	slices.Sort(missing)
	slices.Sort(stale)
	if len(missing) != 0 {
		t.Errorf(
			"%d package-level value(s) of package sdk are not in the census, so this gate CANNOT SEE whether they are errors: %v. Add each one -- a variable with streamAdapterPackageVarOf, a constant with streamAdapterPackageConstOf. If it is an error it then needs a ruling; if it is not, it lands in the printed complement",
			len(missing), missing,
		)
	}
	if len(stale) != 0 {
		t.Errorf(
			"the census names %d value(s) this gate did not enumerate: %v. READ THE REASON ON EACH ONE BEFORE DELETING IT. A name package sdk still declares and this enumeration did not reach is this gate having stopped reading the package, and deleting that entry is the move that hides the sentinel. The enumeration covers package-level var AND const declarations of every production file this build compiles; a name in a file this build does not compile is listed under OUT OF SCOPE and belongs in streamAdapterPlatformValueCensus, beside that file's own build constraint",
			len(stale), stale,
		)
	}
	declared := map[string]error{}
	for name, entry := range census {
		if !entry.declared.Implements(streamAdapterErrorType) {
			complement = append(complement, fmt.Sprintf("%s declared %s, which does not implement error", name, entry.declared.String()))
			continue
		}
		held, ok := entry.value.Interface().(error)
		if !ok || held == nil {
			t.Errorf("%s is declared %s, which implements error, and holds no error value", name, entry.declared.String())
			continue
		}
		declared[name] = held
	}
	for _, line := range outOfScope {
		complement = append(complement, "OUT OF SCOPE: "+line)
	}
	slices.Sort(complement)
	return declared, complement
}

// streamAdapterNameSite is one production site at which package sdk passes a watched name to a
// call or returns it, together with the declaration that site sits in. A site at file scope -- a
// package-level initialiser -- carries no declaration and is treated as reachable, because a gate
// that cannot place a site must not excuse it.
type streamAdapterNameSite struct {
	position    string
	declaration string
}

// streamAdapterNameSitesByDeclaration answers, for each watched name, every production site at
// which package sdk hands it to a call or returns it -- the two ways a declared value becomes an
// error a caller holds -- with the DECLARATION each site sits in recorded beside it, so the
// excuse below can ask WHERE a site is and not only whether one exists. Comparing a value with
// == is not a site: it takes nothing out of this package.
func streamAdapterNameSitesByDeclaration(t *testing.T, names map[string]bool) map[string][]streamAdapterNameSite {
	t.Helper()
	fileSet, parsed, declarations := streamAdapterParse(t)
	enclosing := map[string][]*ast.FuncDecl{}
	positions := map[*ast.FuncDecl]string{}
	for _, declaration := range declarations {
		enclosing[declaration.file] = append(enclosing[declaration.file], declaration.node)
		positions[declaration.node] = declaration.position
	}
	sites := map[string][]streamAdapterNameSite{}
	// one site per SOURCE POSITION: a `return fmt.Errorf("%w", e)` is both a call argument and a
	// return result, and the same octets counted twice read as two escapes
	seen := map[string]bool{}
	for fileName, file := range parsed {
		record := func(expression ast.Expr) {
			ast.Inspect(expression, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if !ok || !names[identifier.Name] {
					return true
				}
				if seen[fileSet.Position(identifier.Pos()).String()] {
					return true
				}
				seen[fileSet.Position(identifier.Pos()).String()] = true
				within := ""
				for _, function := range enclosing[fileName] {
					if function.Pos() <= identifier.Pos() && identifier.Pos() < function.End() {
						within = positions[function]
						break
					}
				}
				sites[identifier.Name] = append(sites[identifier.Name], streamAdapterNameSite{
					position:    fileSet.Position(identifier.Pos()).String(),
					declaration: within,
				})
				return true
			})
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CallExpr:
				for _, argument := range typed.Args {
					record(argument)
				}
			case *ast.ReturnStmt:
				for _, result := range typed.Results {
					record(result)
				}
			}
			return true
		})
	}
	return sites
}

// THE EXCUSE'S OWN NARROWING, AND WHY IT IS NOT "PASSED OR RETURNED NOWHERE".
//
// streamAdapterNonSentinelRulings claims exactly one thing: no error chain the adapter's classify
// can read reaches this value. Until 2026-09-12 that claim was measured as "package sdk passes or
// returns it NOWHERE", which is SUFFICIENT for the claim and is not NECESSARY for it -- and the
// difference is not academic. It is unsatisfiable for every error value of any subsystem that is
// actually used, and the first such subsystem to land in this package (the message-server
// transport) declares six sentinels that are raised, wrapped and returned on a path no store call
// takes. Under the old measurement neither table had a seat for them: not
// streamStoreSentinelRulings, because a verdict there is a claim about a refusal
// SenderRatchet.Next can meet, and not this one, because they are returned. Two sentences that
// cannot both be satisfied, which is the shape this project's own plan tables as unsatisfiable
// AS A PAIR; the repair is to restate the constraint over what the deciding party can read.
//
// WHAT classify CAN READ, derived rather than listed. classify is handed the error a call on the
// adapter's STORE answered, so a value can reach it only if it is raised by a method of a type
// the adapter holds -- the adapter's own type, the store type in its field, and the type of every
// field those declare, transitively -- or by a free function one of those methods calls,
// transitively through free functions. Both sets are read off the syntax tree and both are
// PRINTED with their complements, so the boundary is auditable rather than asserted.
//
// AND THE BOUNDARY IS NAMED RATHER THAN HIDDEN: a method call on a type that is NOT held -- an
// interface field, a value returned by a call -- is not followed. Those calls are counted and
// reported, because an excuse whose measurement has an edge should say where the edge is.

// streamAdapterTypeNamesIn answers every type name an expression mentions, with pointers, slices,
// maps, channels and array element types unwrapped, so that a field declared *StreamStore or
// []streamStoreExclusion is read as the type it holds.
func streamAdapterTypeNamesIn(expression ast.Expr) []string {
	names := []string{}
	ast.Inspect(expression, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok {
			names = append(names, identifier.Name)
		}
		return true
	})
	return names
}

// streamAdapterHeldTypes is the transitive closure of "a type the adapter holds": the adapter
// itself, and the type of every field reachable from it through struct declarations of this
// package.
func streamAdapterHeldTypes(parsed map[string]*ast.File, root string) map[string]bool {
	structs := map[string]*ast.StructType{}
	for _, file := range parsed {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if structType, ok := typeSpec.Type.(*ast.StructType); ok {
					structs[typeSpec.Name.Name] = structType
				}
			}
		}
	}
	held := map[string]bool{}
	queue := []string{root}
	for 0 < len(queue) {
		name := queue[0]
		queue = queue[1:]
		if held[name] {
			continue
		}
		held[name] = true
		structType := structs[name]
		if structType == nil || structType.Fields == nil {
			continue
		}
		for _, field := range structType.Fields.List {
			for _, mentioned := range streamAdapterTypeNamesIn(field.Type) {
				if _, isStruct := structs[mentioned]; isStruct {
					queue = append(queue, mentioned)
				}
			}
		}
	}
	return held
}

// streamAdapterFreeFunctionsFrom is the set of package free functions the methods of the held
// types can call, transitively through free functions. It also answers how many method calls on
// types that are NOT held were passed over, which is this measurement's own edge.
func streamAdapterFreeFunctionsFrom(declarations []streamAdapterDeclaration, held map[string]bool) (map[string]bool, int) {
	free := map[string]streamAdapterDeclaration{}
	for _, declaration := range declarations {
		if declaration.receiver == "" {
			free[declaration.name] = declaration
		}
	}
	reached := map[string]bool{}
	notFollowed := 0
	queue := []streamAdapterDeclaration{}
	for _, declaration := range declarations {
		if held[declaration.receiver] {
			queue = append(queue, declaration)
		}
	}
	seen := map[string]bool{}
	for 0 < len(queue) {
		declaration := queue[0]
		queue = queue[1:]
		if seen[declaration.position] {
			continue
		}
		seen[declaration.position] = true
		ast.Inspect(declaration.node, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch function := call.Fun.(type) {
			case *ast.Ident:
				if next, isFree := free[function.Name]; isFree && !reached[function.Name] {
					reached[function.Name] = true
					queue = append(queue, next)
				}
			case *ast.SelectorExpr:
				// a method call. Followed when it lands on a type this closure already
				// holds -- those declarations are roots already -- and counted otherwise.
				followed := false
				for _, candidate := range declarations {
					if candidate.name == function.Sel.Name && held[candidate.receiver] {
						followed = true
					}
				}
				if !followed {
					notFollowed += 1
				}
			}
			return true
		})
	}
	return reached, notFollowed
}

// THE SCOPE THE VALUE CENSUS IS DEMANDED OVER, AND THE PLATFORM IT IS THERE FOR.
//
// streamAdapterValuePositionsUnder only DEMANDS a census entry for a value declared in a file the
// build compiles, because an entry NAMES the value and a name from a file this build does not
// compile cannot be spelled at all. On windows/amd64 that narrowing removes no NAME: the two
// syscall.Errno constants of the Windows exclusion file are in a file this build compiles, and no
// other constrained production file declares a package-level value. A clause whose effect is
// empty on the platform the suite runs on is a clause nothing drives, and four rounds running
// have shipped one of those.
//
// SO THE PLATFORM IT IS FOR IS REACHED FROM HERE. go/build answers constraints for whatever
// context it is handed, so this case runs the same enumeration under GOOS=linux and watches the
// two Windows constants move OUT of the demanded set and INTO the out-of-scope list -- and watches
// the flock exclusion file, which windows/amd64 does not compile, come into the scope in their
// place. Neither half is asserted as a number; both are asserted as a MOVEMENT between two sets,
// so the case fails if the scoping stops happening in either direction.
//
// Mutations, measured in a disposable copy:
//
//	make streamAdapterBuiltUnder answer true for every file  -> red here: the Windows constants
//	                                                            are demanded under GOOS=linux,
//	                                                            where nothing can name them
//	drop the out-of-scope branch and demand every class member -> the same red
func TestTheValueCensusScopeIsThisBuildsOwnFileSet(t *testing.T) {
	_, parsed, _ := streamAdapterParse(t)
	// THE TWO BUILDS ARE NAMED, NOT READ OFF THE HOST. The property is a movement between a build that
	// compiles the Windows exclusion file and one that does not, and which of the two the host is plays
	// no part in it. This case used to take "here" from build.Default, and its first run on a linux host
	// -- the fork's CI, after the merge of upstream sdk (msgrepo ledger 277) -- failed for that alone.
	windows := build.Default
	windows.GOOS = "windows"
	windows.GOARCH = "amd64"
	here := streamAdapterBuiltUnder(t, parsed, windows)
	excludedHere := []string{}
	for _, name := range slices.Sorted(maps.Keys(here)) {
		if !here[name] {
			excludedHere = append(excludedHere, name)
		}
	}
	t.Logf("SCOPE: go/build says this %s/%s build compiles %d of package sdk's %d production files",
		windows.GOOS, windows.GOARCH, len(parsed)-len(excludedHere), len(parsed))
	t.Logf("COMPLEMENT the build narrowing removed (%d file(s) this build does not compile): %v", len(excludedHere), excludedHere)
	if len(excludedHere) == 0 {
		t.Fatal("go/build excludes no production file of package sdk, so the census scope narrows nothing and this case is measuring nothing")
	}

	// the Windows exclusion file is compiled HERE, so its two constants are demanded here.
	const windowsFile = "message_stream_exclusion_windows.go"
	const flockFile = "message_stream_exclusion_unix.go"
	if _, parsedWindows := parsed[windowsFile]; !parsedWindows {
		t.Fatalf("%s is not among the production files this gate parses, so this case is naming a file that is not there", windowsFile)
	}
	if _, parsedFlock := parsed[flockFile]; !parsedFlock {
		t.Fatalf("%s is not among the production files this gate parses", flockFile)
	}

	namedHere, _, outOfScopeHere, _ := streamAdapterValuePositionsUnder(t, windows)
	elsewhere := build.Default
	elsewhere.GOOS = "linux"
	elsewhere.GOARCH = "amd64"
	namedThere, _, outOfScopeThere, _ := streamAdapterValuePositionsUnder(t, elsewhere)

	if here[flockFile] {
		t.Errorf("go/build says this %s build compiles %s; the two exclusion files' constraints are complements and exactly one of them is this build's", windows.GOOS, flockFile)
	}
	if !here[windowsFile] {
		t.Errorf("go/build says this %s build does not compile %s", windows.GOOS, windowsFile)
	}
	there := streamAdapterBuiltUnder(t, parsed, elsewhere)
	if there[windowsFile] {
		t.Errorf("go/build says a linux build compiles %s", windowsFile)
	}
	if !there[flockFile] {
		t.Errorf("go/build says a linux build does not compile %s", flockFile)
	}

	// THE MOVEMENT, and it is the property: a value declared in a file a build does not compile
	// is REPORTED OUT OF REACH rather than demanded, and the same value is demanded on the build
	// that does compile it.
	moved := 0
	for _, name := range []string{"windowsLockViolation", "windowsSharingViolation"} {
		if _, demandedHere := namedHere[name]; !demandedHere {
			t.Errorf("%s is declared in %s, which this build compiles, and the census is not asked for it here; a value this build can name and does not census is one this gate cannot see the type of", name, windowsFile)
		}
		if _, demandedThere := namedThere[name]; demandedThere {
			t.Errorf(
				"%s is still DEMANDED under a linux build, where no source can name it. A census entry for it could not compile there, so a gate that demands it is a gate that cannot pass on that platform",
				name,
			)
			continue
		}
		found := false
		for _, line := range outOfScopeThere {
			if strings.Contains(line, name) {
				found = true
				t.Logf("  under GOOS=linux: %s", line)
			}
		}
		if !found {
			t.Errorf("%s is neither demanded nor reported out of scope under a linux build, so this gate dropped it silently, which is the one outcome a scope narrowing must never have", name)
			continue
		}
		moved += 1
	}
	if moved == 0 {
		t.Fatal("no value moved between the two builds' demanded sets, so the build scoping is inert and this case is holding nothing")
	}
	t.Logf("OUT OF THIS BUILD'S REACH: %d name(s) here, %d under GOOS=linux; %d value(s) moved between the two",
		len(outOfScopeHere), len(outOfScopeThere), moved)
	t.Log("the scope narrowing removes no NAME on windows/amd64 and removes two on every GOOS that does not build the Windows exclusion file. That is why it takes a context rather than reading build.Default: the platform it exists for is not the platform this suite runs on")
}

// CLASS: every error sentinel package sdk DECLARES -- every package-level variable WHOSE DECLARED
// TYPE IS AN ERROR. Not "initialised by errors.New with a string literal", which is what this
// class used to be and which a sentinel spelled fmt.Errorf, errors.New(aConst), &aType{} or a
// plain `var x error = ...` walks straight past. The enumeration is over var NAMES off the syntax
// tree, which no initialiser shape can hide, and the classification is reflect's answer to
// Type.Implements(error), which no initialiser shape can lie to. It is deliberately the whole
// package and not a name prefix: the brief asks that a later sentinel must not silently pass
// through, and a class narrowed to names containing "Stream" would let a sentinel spelled any
// other way do exactly that. Over-reach is the safe direction here -- an unrelated sentinel added
// to sdk fails this gate until somebody rules on it, which is a decision being asked for rather
// than skipped.
//
// SCOPE, derived separately: the declarations, not the uses. What a gate over uses would answer is
// which sentinels the store RAISES; what this has to answer is which sentinels EXIST, because the
// one that reaches the adapter unclassified is the one nothing raised yet.
func TestTheStoreSentinelClassIsTotalOverTheAdaptersMapping(t *testing.T) {
	declared, complement := streamAdapterSentinelDeclarations(t)
	named, _, outOfScope, _ := streamAdapterPackageValuePositions(t)
	t.Logf("SCOPE: %d named package-level value(s) of package sdk -- var AND const -- enumerated off the syntax tree, over the production files go/build says this build compiles; %d name(s) are out of this build's reach", len(named), len(outOfScope))
	t.Logf("CLASS: %d of them are declared as an error: %v", len(declared), slices.Sorted(maps.Keys(declared)))
	t.Logf("COMPLEMENT the is-an-error narrowing removed (%d):", len(complement))
	for _, line := range complement {
		t.Logf("    %s", line)
	}
	if len(declared) == 0 {
		t.Fatal("this gate found no error sentinel at all, so it is holding nothing")
	}
	if len(complement) == 0 {
		t.Error("the complement is empty, which means the is-an-error narrowing removed no package-level variable at all and this gate is not the gate it says it is")
	}
	if len(declared)+len(complement) < len(streamAdapterCensus()) {
		t.Errorf(
			"the class (%d) and the complement (%d) do not cover the census (%d); a named value that is in neither is one this gate silently dropped",
			len(declared), len(complement), len(streamAdapterCensus()),
		)
	}

	ruled := map[string]streamStoreSentinelRuling{}
	for _, ruling := range streamStoreSentinelRulings {
		if _, already := ruled[ruling.name]; already {
			t.Errorf("the adapter's mapping names %s twice", ruling.name)
		}
		ruled[ruling.name] = ruling
	}
	// THE CLASS IS TOTAL OVER TWO TABLES AND THEY ARE DISJOINT. The adapter's own mapping
	// rules the sentinels a store failure can carry; the second table rules the package-level
	// error values that are NOT the store's sentinels and cannot reach classify at all. The
	// second is in this file rather than in production because a verdict in
	// streamStoreSentinelRulings is a claim about a refusal SenderRatchet.Next can meet.
	nonSentinels := streamAdapterNonSentinels()
	unclassified := []string{}
	for name := range declared {
		_, isRuled := ruled[name]
		_, isNonSentinel := nonSentinels[name]
		if isRuled && isNonSentinel {
			t.Errorf("%s is ruled BOTH as one of the adapter's sentinels and as a value that cannot reach it; the two tables must be disjoint", name)
		}
		if !isRuled && !isNonSentinel {
			unclassified = append(unclassified, name)
		}
	}
	stale := []string{}
	for name := range ruled {
		if _, ok := declared[name]; !ok {
			stale = append(stale, name)
		}
	}
	staleNonSentinels := []string{}
	for name := range nonSentinels {
		if _, ok := declared[name]; !ok {
			staleNonSentinels = append(staleNonSentinels, name)
		}
	}
	slices.Sort(unclassified)
	slices.Sort(stale)
	slices.Sort(staleNonSentinels)
	if len(unclassified) != 0 {
		t.Errorf(
			"%d error value(s) package sdk declares carry no ruling at all: %v. An unclassified sentinel is forwarded as TRANSIENT, so a permanent refusal spelled this way would tell SenderRatchet.Next to retry forever and pay a durable write per attempt. Rule it in streamStoreSentinelRulings if a store failure can carry it, or in streamAdapterNonSentinelRulings if it cannot -- and the second table is CHECKED, not taken on trust",
			len(unclassified), unclassified,
		)
	}
	if len(stale) != 0 {
		t.Errorf("the adapter's mapping rules on %d name(s) package sdk no longer declares as an error: %v", len(stale), stale)
	}
	if len(staleNonSentinels) != 0 {
		t.Errorf("streamAdapterNonSentinelRulings rules on %d name(s) package sdk no longer declares as an error: %v", len(staleNonSentinels), staleNonSentinels)
	}

	// AND THE SECOND TABLE'S EXCUSE IS MEASURED. A value that cannot reach classify is a value
	// this package never hands to a call and never returns -- those are the two ways a
	// declared value becomes an error a caller holds. Comparing one with == does not.
	watched := map[string]bool{}
	for name := range nonSentinels {
		watched[name] = true
	}
	if len(watched) != 0 {
		_, allFiles, allDeclarations := streamAdapterParse(t)
		adapterType := streamAdapterTypeName(t)
		held := streamAdapterHeldTypes(allFiles, adapterType)
		reachableFree, notFollowed := streamAdapterFreeFunctionsFrom(allDeclarations, held)

		receivers := map[string]bool{}
		freeCount := 0
		for _, declaration := range allDeclarations {
			if declaration.receiver == "" {
				freeCount += 1
				continue
			}
			receivers[declaration.receiver] = true
		}
		otherReceivers := []string{}
		for name := range receivers {
			if !held[name] {
				otherReceivers = append(otherReceivers, name)
			}
		}
		slices.Sort(otherReceivers)
		t.Logf("WHAT classify CAN READ, derived: %d type(s) the adapter holds, transitively through struct fields from %s: %v",
			len(held), adapterType, slices.Sorted(maps.Keys(held)))
		t.Logf("    and %d free function(s) their methods call, transitively: %v", len(reachableFree), slices.Sorted(maps.Keys(reachableFree)))
		t.Logf("COMPLEMENT the held-type narrowing removed: %d receiver type(s) package sdk declares methods on that the adapter does not hold: %v",
			len(otherReceivers), otherReceivers)
		t.Logf("COMPLEMENT the free-function narrowing removed: %d of %d free function(s) no held type calls", freeCount-len(reachableFree), freeCount)
		t.Logf("THIS MEASUREMENT'S OWN EDGE, named rather than hidden: %d method call(s) inside held types land on a type that is not held and are not followed", notFollowed)
		if len(held) == 0 || len(otherReceivers) == 0 {
			t.Error("the held-type narrowing removed no receiver type at all, so every excuse below is measured against a set that excludes nobody")
		}
		if len(reachableFree) == 0 {
			t.Error("no free function at all is reachable from the held types, which means the call walk found nothing and the free-function half of this measurement is inert")
		}

		byPosition := map[string]streamAdapterDeclaration{}
		for _, declaration := range allDeclarations {
			byPosition[declaration.position] = declaration
		}
		escaped := streamAdapterNameSitesByDeclaration(t, watched)
		for _, name := range slices.Sorted(maps.Keys(watched)) {
			within := []string{}
			beyond := []string{}
			for _, site := range escaped[name] {
				declaration, placed := byPosition[site.declaration]
				reads := !placed ||
					held[declaration.receiver] ||
					(declaration.receiver == "" && reachableFree[declaration.name])
				if reads {
					within = append(within, site.position)
					continue
				}
				beyond = append(beyond, fmt.Sprintf("%s in %s", site.position, declaration.label()))
			}
			slices.Sort(within)
			slices.Sort(beyond)
			if len(within) != 0 {
				t.Errorf(
					"%s is ruled as an error value that cannot reach the adapter, and production sdk passes or returns it at %v, which classify CAN read: those sites sit in a method of a type the adapter holds, in a free function its methods call, or at file scope where this gate cannot place them. That excuse no longer holds and it needs a real verdict in streamStoreSentinelRulings",
					name, within,
				)
				continue
			}
			if len(beyond) == 0 {
				t.Logf("  %-32s NOT A STORE SENTINEL, and measured so: package sdk never passes or returns it. %s", name, nonSentinels[name])
				continue
			}
			t.Logf("  %-32s NOT A STORE SENTINEL, and measured so: passed or returned at %d site(s) %v, not one of them anywhere classify can read. %s",
				name, len(beyond), beyond, nonSentinels[name])
		}
	}

	// and the ruling is bound to the VALUE and not only to the name -- by IDENTITY now rather
	// than by message. Two sentinels spelled errors.New with the same string are two distinct
	// values that errors.Is tells apart and a message comparison does not, so a ruling can be
	// attached to the wrong one of them and still read as correct. This compares the value the
	// package declares under that name with the value the ruling holds.
	for _, ruling := range streamStoreSentinelRulings {
		held, ok := declared[ruling.name]
		if !ok {
			continue
		}
		if ruling.sentinel == nil {
			t.Errorf("the ruling for %s carries a nil sentinel", ruling.name)
			continue
		}
		if ruling.sentinel != held {
			t.Errorf(
				"the ruling named %s holds the value %q and package sdk declares %q under that name; they are not the same value, so the ruling classifies a refusal nobody raises and leaves the one it was written for unclassified",
				ruling.name, ruling.sentinel, held,
			)
		}
		if !errors.Is(held, ruling.sentinel) {
			t.Errorf("errors.Is cannot find the ruling for %s in the value package sdk declares under that name", ruling.name)
		}
	}
	permanent, transient, rewound := 0, 0, 0
	for _, ruling := range streamStoreSentinelRulings {
		if ruling.permanent {
			permanent += 1
		} else {
			transient += 1
		}
		if ruling.rewound {
			rewound += 1
		}
		t.Logf("  %-30s permanent=%-5v rewound=%-5v %s", ruling.name, ruling.permanent, ruling.rewound, ruling.ruling)
	}
	t.Logf("VERDICTS: %d permanent, %d transient, %d rewound", permanent, transient, rewound)
	if permanent == 0 || transient == 0 {
		t.Error("the mapping rules every sentinel the same way, so the exactness case below cannot tell the two answers apart and neither can a ratchet")
	}
}

// The mapping is exact IN BOTH DIRECTIONS, and the two errors it must not make are opposites.
//
// A PERMANENT refusal forwarded as transient tells SenderRatchet.Next to retry a wedged ladder
// forever, paying a durable write per attempt. A TRANSIENT one forwarded as permanent wedges a
// healthy ladder over a full disk, which is the case connect/messagegroup's own ratchet names as
// the one that must stay a retry. This drives every member of the class, and both directions.
func TestNoPermanentRefusalIsForwardedAsTransientAndNoTransientOneIsWedged(t *testing.T) {
	reserver, _ := streamAdapterTestReserver(t)
	concrete := streamAdapterTestConcrete(t, reserver)

	// the reviewer's finding, executable: not one of the store's own sentinels is findable as
	// either messagegroup name, so an adapter that forwarded a store error unchanged would
	// classify the store's PERMANENT refusal as retryable.
	for _, ruling := range streamStoreSentinelRulings {
		if errors.Is(ruling.sentinel, messagegroup.ErrStreamIndexConsumed) {
			t.Errorf("%s is already findable as messagegroup.ErrStreamIndexConsumed, so the mapping this adapter performs is not load-bearing and this case is measuring nothing", ruling.name)
		}
		if errors.Is(ruling.sentinel, messagegroup.ErrStreamIndexRewound) {
			t.Errorf("%s is already findable as messagegroup.ErrStreamIndexRewound", ruling.name)
		}
	}

	for _, ruling := range streamStoreSentinelRulings {
		t.Run(ruling.name, func(t *testing.T) {
			raised := fmt.Errorf("row deadbeef: %w", ruling.sentinel)
			mapped := concrete.classify(raised)
			if mapped == nil {
				t.Fatal("the mapping answered nil for a refusal")
			}
			if !errors.Is(mapped, ruling.sentinel) {
				t.Errorf("the mapped error does not carry %s underneath; the store's own refusal must survive the wrap, or a caller that wanted to know WHICH refusal it met has lost it", ruling.name)
			}
			if !strings.Contains(mapped.Error(), "row deadbeef") {
				t.Errorf("the mapped error is %q and does not carry the store's own message", mapped.Error())
			}
			consumed := errors.Is(mapped, messagegroup.ErrStreamIndexConsumed)
			if consumed != ruling.permanent {
				t.Errorf(
					"%s maps to errors.Is(messagegroup.ErrStreamIndexConsumed) = %v and its ruling says permanent = %v. SenderRatchet.Next reads exactly that call: true stops the ladder forever, false retries it. The ruling is %q",
					ruling.name, consumed, ruling.permanent, ruling.ruling,
				)
			}
			rewound := errors.Is(mapped, messagegroup.ErrStreamIndexRewound)
			if rewound != ruling.rewound {
				t.Errorf("%s maps to errors.Is(messagegroup.ErrStreamIndexRewound) = %v and its ruling says rewound = %v", ruling.name, rewound, ruling.rewound)
			}
		})
	}

	// the control: an error carrying no member of the class is forwarded, and it is forwarded as
	// transient. That is safe only because the class above is total.
	foreign := errors.New("a refusal no sentinel of this package names")
	mapped := concrete.classify(foreign)
	if !errors.Is(mapped, foreign) {
		t.Error("an unclassified refusal lost its cause")
	}
	if errors.Is(mapped, messagegroup.ErrStreamIndexConsumed) || errors.Is(mapped, messagegroup.ErrStreamIndexRewound) {
		t.Errorf("an unclassified refusal was given a messagegroup sentinel: %v", mapped)
	}
	if mapped := concrete.classify(nil); mapped != nil {
		t.Errorf("the mapping answered %v for a nil error", mapped)
	}
}

// TWO OF THE EIGHT RULINGS ARE UNREACHABLE, and the unreachability is measured here rather than
// asserted in a comment beside them.
//
// This is what the mapping's own mutation set turned up. Flipping ErrStreamStoreLocked's verdict
// and flipping errStreamInjectedFlushFailure's verdict each left the whole stream and adapter
// suite green, where flipping any of the other six turned it red -- so those two verdicts are not
// answers anything consults, and a comment claiming they were would be exactly the undriven clause
// this pass exists to remove. They stay in the table because the class must be TOTAL: a sentinel
// with no ruling is forwarded as transient, and totality is what keeps a permanent refusal out of
// that bucket. What they do not get is a claim that something checks them.
//
// The reasons are different and both are observable.
func TestTheTwoRulingsNoErrorChainReaches(t *testing.T) {
	reserver, store := streamAdapterTestReserver(t)
	key, _ := streamAdapterTestKey(t, 0x89)

	// errStreamInjectedFlushFailure: writeOneRecord formats it with %v and not %w, so it is in
	// the MESSAGE and not in the CHAIN, and errors.Is cannot find it.
	streamTestSetInterrupt(t, store, streamAppendFailTheFlush)
	_, err := reserver.Reserve(key)
	if err == nil {
		t.Fatal("a failed flush was not refused")
	}
	if errors.Is(err, errStreamInjectedFlushFailure) {
		t.Errorf("a failed flush IS findable as errStreamInjectedFlushFailure (%v), so its ruling is reachable after all and the table's own note is stale", err)
	}
	if !strings.Contains(err.Error(), errStreamInjectedFlushFailure.Error()) {
		t.Errorf("a failed flush does not even carry the injected failure's text: %v", err)
	}
	if !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("a failed flush is classified through %v instead", err)
	}
	t.Logf("errStreamInjectedFlushFailure is in the MESSAGE and not in the CHAIN: %v", err)

	// ErrStreamStoreLocked: OpenStreamStore raises it, and a reserver is built from a store
	// that is already open, so it cannot travel through either of the adapter's methods.
	second, err := OpenStreamStore(store.dir)
	if !errors.Is(err, ErrStreamStoreLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("a second store over one directory answered %v, want ErrStreamStoreLocked", err)
	}
	if second != nil {
		t.Error("a refused open answered a store as well as an error")
	}
	if reserver := NewStreamIndexReserver(second); reserver != nil {
		t.Error("a reserver was built over the store a locked open did not produce")
	}
	t.Logf("ErrStreamStoreLocked is raised before a reserver exists: %v", err)
}

// The two seats, through the REAL store rather than through a synthesised error. A rewind seen by
// a reader is ErrStreamIndexRewound and is NOT permanent; the same rewind met by an allocator is
// both, because the next position is one the store has already returned.
//
// Ledger item 171 turns on the difference, and mapping a rewind onto the consumed name alone would
// wedge a ladder whose store merely answered a query.
func TestTheReaderSeatIsRewoundAloneAndTheAllocatorSeatIsAlsoConsumed(t *testing.T) {
	reserver, store := streamAdapterTestReserver(t)
	key, _ := streamAdapterTestKey(t, 0x82)
	rowName := streamRowName(key)
	for want := uint64(1); want <= 3; want += 1 {
		if index, err := reserver.Reserve(key); err != nil || index != want {
			t.Fatalf("reserve answered (%d, %v), want (%d, nil)", index, err, want)
		}
	}
	path := filepath.Join(store.rowDir, rowName)
	if length := streamTestRowLength(t, path); length != 3*streamRecordWidth {
		t.Fatalf("the row is %d octets, want %d", length, 3*streamRecordWidth)
	}
	if err := os.Truncate(path, streamRecordWidth); err != nil {
		t.Fatalf("truncate the row: %v", err)
	}
	if length := streamTestRowLength(t, path); length != streamRecordWidth {
		t.Fatalf("the truncation did not land: the row is %d octets", length)
	}

	highWater, err := reserver.HighWater(key)
	if !errors.Is(err, messagegroup.ErrStreamIndexRewound) {
		t.Errorf("HighWater answered (%d, %v) for a row that went backwards, want messagegroup.ErrStreamIndexRewound", highWater, err)
	}
	if errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
		t.Errorf("HighWater answered %v, which errors.Is finds ErrStreamIndexConsumed in. The reader's seat reports that the number moved; claiming the ladder is permanently wedged is the allocator's sentence and a ratchet built on this query would refuse every send for the life of the session", err)
	}
	if highWater != 0 {
		t.Errorf("a refused query answered %d beside its error", highWater)
	}

	index, err := reserver.Reserve(key)
	if !errors.Is(err, messagegroup.ErrStreamIndexRewound) {
		t.Errorf("Reserve answered (%d, %v), want messagegroup.ErrStreamIndexRewound", index, err)
	}
	if !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
		t.Errorf("Reserve answered %v, which errors.Is does not find ErrStreamIndexConsumed in; the allocator's next position is one it has already returned and no later call can make another", err)
	}
	if !errors.Is(err, ErrStreamStoreRewound) {
		t.Errorf("the mapped refusal lost the store's own sentinel: %v", err)
	}
	if index != 0 {
		t.Errorf("a refused allocation answered index %d beside its error", index)
	}
}

// A transient store failure is NOT the permanent refusal, and the ladder it happened on is still
// allocatable afterwards. This is the control for a mapping that over-reports: a mapping that
// called every failure permanent would wedge a healthy ladder over one failed flush.
func TestATransientStoreFailureIsNotTheConsumedSentinel(t *testing.T) {
	reserver, store := streamAdapterTestReserver(t)
	key, _ := streamAdapterTestKey(t, 0x83)

	streamTestSetInterrupt(t, store, streamAppendFailTheFlush)
	index, err := reserver.Reserve(key)
	if err == nil {
		t.Fatalf("a failed flush answered (%d, nil)", index)
	}
	if errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
		t.Errorf("a failed flush answered %v, which errors.Is finds ErrStreamIndexConsumed in; connect/messagegroup's own ratchet names a full disk as the case that must stay a retry, and a ladder wedged on one never sends again", err)
	}
	if errors.Is(err, messagegroup.ErrStreamIndexRewound) {
		t.Errorf("a failed flush answered %v, which errors.Is finds ErrStreamIndexRewound in", err)
	}
	if !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("the mapped refusal lost the store's own sentinel: %v", err)
	}
	if index != 0 {
		t.Errorf("a refused allocation answered index %d beside its error", index)
	}

	streamTestSetInterrupt(t, store, streamAppendUninterrupted)
	next, err := reserver.Reserve(key)
	if err != nil {
		t.Fatalf("the retry after a transient failure was refused: %v; a transient refusal a retry cannot clear is a permanent one wearing the wrong name", err)
	}
	if next != 2 {
		t.Errorf("the retry answered %d, want 2; the index the failed flush wrote is burned, and the server enforces monotonicity and not contiguity", next)
	}

	// the other synthetic interruption, so that BOTH of the mapping's unexported rulings are
	// driven through the adapter rather than only asserted against themselves.
	streamTestSetInterrupt(t, store, streamAppendTearBeforeFlush)
	torn, err := reserver.Reserve(key)
	if err == nil {
		t.Fatalf("an interrupted append answered (%d, nil)", torn)
	}
	if errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
		t.Errorf("an interrupted append answered %v, which errors.Is finds ErrStreamIndexConsumed in; the next append overwrites the torn tail in place and succeeds, so it is a retry", err)
	}
	if !errors.Is(err, errStreamAppendInterrupted) {
		t.Errorf("the mapped refusal lost the interruption underneath: %v", err)
	}
	streamTestSetInterrupt(t, store, streamAppendUninterrupted)
	if after, err := reserver.Reserve(key); err != nil || after != 3 {
		t.Errorf("the retry after an interrupted append answered (%d, %v), want (3, nil)", after, err)
	}
}

// A ROW THIS BUILD CANNOT KEY IS PERMANENT, through the adapter. Ledger item 170's refusal is
// deliberately coarse -- one foreign-tagged row refuses every key in the directory -- and no retry
// rewrites it, so a ratchet told to keep asking would pay a durable write per attempt against a
// directory that will answer the same way forever.
func TestARowOfAnotherKeySpaceIsPermanentThroughTheAdapter(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x88)
	key, err := streamKeyFromOctets(parts...)
	if err != nil {
		t.Fatal(err)
	}
	preA1 := streamKeyPreA1{}
	copy(preA1.GroupId[:], parts[0])
	copy(preA1.SenderHandle[:], parts[1])
	preA1.RetentionWire = 1
	foreignName := streamRowNameOf(reflect.ValueOf(preA1))
	if strings.HasPrefix(foreignName, streamKeySpaceTagOf(streamKeyType())) {
		t.Fatalf("the pre-A1 field set derives this build's own key-space tag, so this case cannot plant a foreign row")
	}
	streamTestPlantRow(t, dir, foreignName, streamTestRowBody(foreignName, 1, 2, 3))
	reserver := NewStreamIndexReserver(streamTestOpen(t, dir))

	for attempt := 1; attempt <= 3; attempt += 1 {
		index, err := reserver.Reserve(key)
		if !errors.Is(err, ErrStreamKeySpace) {
			t.Fatalf("attempt %d answered (%d, %v), want ErrStreamKeySpace underneath", attempt, index, err)
		}
		if !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
			t.Errorf("attempt %d answered %v, which errors.Is does not find ErrStreamIndexConsumed in; a foreign key space is not a state a retry leaves", err, attempt)
		}
	}
	if highWater, err := reserver.HighWater(key); !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
		t.Errorf("the query answered (%d, %v), want the permanent sentinel", highWater, err)
	}
}

// The store's PERMANENT refusal reaches a ratchet as the name it branches on, over the real store.
func TestTheStoresPermanentRefusalReachesTheRatchetAsTheConsumedSentinel(t *testing.T) {
	dir := t.TempDir()
	key, parts := streamAdapterTestKey(t, 0x84)
	rowName := streamRowName(key)
	streamTestPlantRow(t, dir, rowName, streamTestRowBody(rowName, ^uint64(0)))
	store := streamTestOpen(t, dir)
	reserver := NewStreamIndexReserver(store)

	if highWater, err := reserver.HighWater(key); err != nil || highWater != ^uint64(0) {
		t.Fatalf("high water answered (%d, %v), want (%d, nil)", highWater, err, ^uint64(0))
	}
	for attempt := 1; attempt <= 3; attempt += 1 {
		index, err := reserver.Reserve(key)
		if !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
			t.Fatalf("attempt %d answered (%d, %v), want messagegroup.ErrStreamIndexConsumed", attempt, index, err)
		}
		if !errors.Is(err, ErrStreamStoreConsumed) {
			t.Errorf("attempt %d lost the store's own sentinel: %v", attempt, err)
		}
	}
	// the same store, reached without the adapter, answers an error no ratchet can classify.
	_, raw := store.ReserveStreamIndex(parts[0], parts[1])
	if errors.Is(raw, messagegroup.ErrStreamIndexConsumed) {
		t.Error("the store's own refusal is already findable as messagegroup.ErrStreamIndexConsumed, so this adapter's mapping is not load-bearing")
	}
	t.Logf("WITHOUT the adapter, the same permanent refusal is %v -- errors.Is(messagegroup.ErrStreamIndexConsumed) = %v, which SenderRatchet.Next reads as RETRYABLE and would retry forever, paying a durable write per attempt",
		raw, errors.Is(raw, messagegroup.ErrStreamIndexConsumed))
}

// ----------------------------------------------------------------------------------------------
// Property 4 -- a wrong-width StreamKey cannot exist, and a wrong-width []byte cannot pass.
// ----------------------------------------------------------------------------------------------

// The StreamKey direction is total by construction, which is what the array types buy; this is the
// other half, re-checked AT THE ADAPTER so it is safe when it is the entry point rather than the
// exit. A flattening that truncated a field would otherwise reach the store as two well-formed
// slices naming a row that is not this stream's.
func TestAWrongWidthKeyParameterIsRefusedAtTheAdapter(t *testing.T) {
	reserver, _ := streamAdapterTestReserver(t)
	concrete := streamAdapterTestConcrete(t, reserver)
	key, parts := streamAdapterTestKey(t, 0x85)

	if err := concrete.refuseWrongWidth(parts); err != nil {
		t.Fatalf("the width check refused a correct flattening: %v", err)
	}
	keyType := streamKeyType()
	t.Logf("CLASS: %d field(s) of %s, each with its own width, read through reflection: this gate offers a wrong width for each in turn", keyType.NumField(), keyType.String())

	for i := range keyType.NumField() {
		width := keyType.Field(i).Type.Len()
		for _, wrong := range []int{width - 1, width + 1, 0} {
			bad := [][]byte{}
			for j, part := range parts {
				if j == i {
					bad = append(bad, make([]byte, wrong))
					continue
				}
				bad = append(bad, part)
			}
			err := concrete.refuseWrongWidth(bad)
			if !errors.Is(err, ErrStreamKeyWidth) {
				t.Errorf("a %d-octet parameter %d (the field is %d wide) answered %v, want ErrStreamKeyWidth at the adapter; a short key padded or a long key truncated collides two streams onto one row", wrong, i, width, err)
			}
			if mapped := concrete.classify(err); !errors.Is(mapped, messagegroup.ErrStreamIndexConsumed) {
				t.Errorf("the adapter's width refusal maps to %v, which is not permanent; a key of the wrong width is the wrong width on every retry", mapped)
			}
		}
	}

	// and a parameter COUNT that is not the count the store takes.
	short := parts[:len(parts)-1]
	if err := concrete.refuseWrongWidth(short); !errors.Is(err, ErrStreamKeyWidth) {
		t.Errorf("a flattening of %d parameter(s) answered %v, want ErrStreamKeyWidth", len(short), err)
	}
	long := append(append([][]byte{}, parts...), make([]byte, 8))
	if err := concrete.refuseWrongWidth(long); !errors.Is(err, ErrStreamKeyWidth) {
		t.Errorf("a flattening of %d parameter(s) answered %v, want ErrStreamKeyWidth", len(long), err)
	}
	// the correct key still allocates, so the check above refuses nothing it should not.
	if index, err := reserver.Reserve(key); err != nil || index != 1 {
		t.Errorf("a correct key answered (%d, %v), want (1, nil)", index, err)
	}

	// AND THE CHECK IS WIRED IN, which no input can drive: the StreamKey direction is total by
	// construction, so on a correct build nothing a caller can pass reaches this refusal through
	// the flattening. Deleting the CALL would therefore leave every case above green while the
	// adapter stopped checking anything -- which is the undriven-clause shape this whole pass is
	// about -- so the wiring is read off the syntax tree instead. Both ends are identified by
	// SHAPE and neither is named: the producer is the adapter method that hands octets out, the
	// checker is the adapter method that takes them and answers only an error.
	adapterType := streamAdapterTypeName(t)
	_, _, declarations := streamAdapterParse(t)
	producers := []streamAdapterDeclaration{}
	checkers := []streamAdapterDeclaration{}
	for _, declaration := range declarations {
		if declaration.receiver != adapterType {
			continue
		}
		results := streamAdapterResultTypes(declaration.node)
		parameters := []string{}
		if declaration.node.Type.Params != nil {
			for _, parameter := range declaration.node.Type.Params.List {
				parameters = append(parameters, streamAdapterTypeText(parameter.Type))
			}
		}
		if slices.Contains(results, "[][]byte") {
			producers = append(producers, declaration)
		}
		if len(parameters) == 1 && parameters[0] == "[][]byte" && len(results) == 1 && results[0] == "error" {
			checkers = append(checkers, declaration)
		}
	}
	t.Logf("the flattening's producers (%d) and the width checkers (%d), both read by shape off the adapter's method set", len(producers), len(checkers))
	if len(producers) == 0 || len(checkers) == 0 {
		t.Fatalf("the wiring gate found %d producer(s) and %d checker(s), so it is holding nothing", len(producers), len(checkers))
	}
	names := map[string]bool{}
	for _, checker := range checkers {
		names[checker.name] = true
	}
	for _, producer := range producers {
		calls := streamAdapterCallsSelector(producer.node, names)
		if len(calls) == 0 {
			t.Errorf(
				"%s hands octets out and calls none of the adapter's width checks %v; the StreamKey direction is total by construction, so a flattening that skipped the check would pass every input this gate can offer and would hand the store a truncated key that names another stream's row",
				producer.label(), slices.Sorted(maps.Keys(names)),
			)
			continue
		}
		t.Logf("  %s calls %v", producer.label(), calls)
	}
}

// ----------------------------------------------------------------------------------------------
// the interface's own contract, through the adapter rather than through the store
// ----------------------------------------------------------------------------------------------

// Clause 4: the store is total over its key space, so a stream never seen answers 0 with no error
// and the first allocation is 1. Clause 5: Reserve is not idempotent; two calls are two indices.
// Clause 2: HighWater never rewinds across a restart.
func TestTheAdapterIsTotalOverAStreamNeverSeenAndNeverRewindsAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	reserver := NewStreamIndexReserver(store)
	first, _ := streamAdapterTestKey(t, 0x86)
	second, _ := streamAdapterTestKey(t, 0x87)

	highWater, err := reserver.HighWater(first)
	if err != nil || highWater != 0 {
		t.Fatalf("a stream never seen answered (%d, %v), want (0, nil); the absence of an error is what clause 4 requires", highWater, err)
	}
	seen := map[uint64]bool{}
	for want := uint64(1); want <= 4; want += 1 {
		index, err := reserver.Reserve(first)
		if err != nil || index != want {
			t.Fatalf("allocation %d answered (%d, %v)", want, index, err)
		}
		if seen[index] {
			t.Fatalf("index %d was handed out twice", index)
		}
		seen[index] = true
	}
	// a second stream is a second counter: a shared one would answer 5 here.
	if index, err := reserver.Reserve(second); err != nil || index != 1 {
		t.Errorf("the first allocation of a second stream answered (%d, %v), want (1, nil); two streams sharing one row is two ladders on one counter", index, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := streamTestOpen(t, dir)
	restarted := NewStreamIndexReserver(reopened)
	if highWater, err := restarted.HighWater(first); err != nil || highWater != 4 {
		t.Errorf("after a restart the first stream answered (%d, %v), want (4, nil); the ratchet resumes at highWater + 1 and never at a recomputed value", highWater, err)
	}
	if index, err := restarted.Reserve(first); err != nil || index != 5 {
		t.Errorf("the first allocation after a restart answered (%d, %v), want (5, nil)", index, err)
	}
}

// A nil store answers a nil reserver, and the refusal that owes is the one that already exists.
func TestANilStoreAnswersNoReserverAndNewGroupSessionRefusesIt(t *testing.T) {
	reserver := NewStreamIndexReserver(nil)
	if reserver != nil {
		t.Fatalf("NewStreamIndexReserver(nil) answered %#v, want a nil interface; a reserver over no store is a seal gated by nothing", reserver)
	}
	_, err := messagegroup.NewGroupSession(streamAdapterRealGroup(t), []byte("not the reserver under test"), nil,
		reserver, func() int64 { return 1 }, []byte("nonce"))
	if !errors.Is(err, messagegroup.ErrNilStreamIndexReserver) {
		t.Fatalf("NewGroupSession over a nil reserver answered %v, want ErrNilStreamIndexReserver; section 5.6 has the constructor take the sink to make it explicit, and that refusal is what gates every seal", err)
	}
	t.Logf("the refusal a nil store lands on, and it is messagegroup's rather than a second one here: %v", err)
}

// ----------------------------------------------------------------------------------------------
// Property 4 -- the forever-retry, stopped where the store can tell and measured where it cannot.
// ----------------------------------------------------------------------------------------------

// A CORRUPT ROW STOPS THE LADDER ON THE FIRST ATTEMPT, THROUGH THE ADAPTER.
//
// The adapter rules the whole ErrStreamStoreState class TRANSIENT, and that ruling is right for a
// failed flush and a full disk: connect/messagegroup's own ratchet names those as the cases that
// must stay a retry. It is wrong for a row whose body did not classify at open, and wrong in the
// expensive direction -- SenderRatchet.Next would go on asking forever, paying a durable write per
// attempt, against a row that will never accept one.
//
// The repair is the STORE's and not this file's: an adapter cannot invent a discriminator the
// value does not carry, so the store raises ErrStreamStoreConsumed BESIDE ErrStreamStoreState for
// exactly the sub-class whose permanence is knowable from inside it. classify's permanent||...
// then finds it with no change to the ruling table, which is the shape of a correct repair here.
//
// THE 200 ATTEMPTS SPLIT 3 / 197 by design: the corrupt row's bytes are removed after attempt 3,
// so attempts 1-3 are answered by the branch that reads them and attempts 4-200 by the branch that
// has only the store's own entry left. Both branches are driven, and each mutation is measured in
// a disposable copy against exactly its own share:
//
//	drop ErrStreamStoreConsumed where the bytes are still read  -> 3 of 200 lose it
//	drop it where only the store's entry is left                -> 197 of 200 lose it
//	delete repairRow's unreadable marker altogether              -> 197 of 200 ALLOCATE,
//	                                                                starting again at index 1
func TestACorruptRowIsPermanentThroughTheAdapterAndNotRetriedForever(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x91)
	rowName := streamTestRowName(t, parts)
	key, err := streamKeyFromOctets(parts...)
	if err != nil {
		t.Fatalf("build the stream key: %v", err)
	}
	body := streamTestRowBody(rowName, 1, 2, 3)
	streamTestCorruptRecord(body, 2)
	path := streamTestPlantRow(t, dir, rowName, body)
	reserver := NewStreamIndexReserver(streamTestOpen(t, dir))

	const attempts = 200
	permanent, allocated := 0, 0
	for attempt := 1; attempt <= attempts; attempt += 1 {
		index, err := reserver.Reserve(key)
		if err == nil {
			allocated += 1
			t.Errorf("attempt %d allocated index %d on a row this store could not read at open", attempt, index)
			continue
		}
		if index != 0 {
			t.Errorf("attempt %d answered index %d beside its error", attempt, index)
		}
		if errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
			permanent += 1
		}
		if attempt == 1 {
			t.Logf("attempt 1: %v", err)
		}
		// and the row's bytes go away under the store after three attempts, which is the
		// state that used to read as a stream never seen.
		if attempt == 3 {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove the corrupt row: %v", err)
			}
		}
	}
	if allocated != 0 {
		t.Fatalf("%d of %d attempts ALLOCATED an index on a row this store could not read at open", allocated, attempts)
	}
	if permanent != attempts {
		t.Errorf("%d of %d refusals carried messagegroup.ErrStreamIndexConsumed; the rest read as transient, and a ratchet reading a transient refusal asks again -- forever, against a row that will never accept a record", permanent, attempts)
	}
	if highWater, err := reserver.HighWater(key); !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
		t.Errorf("the query answered (%d, %v), want the permanent sentinel; the reader's seat cannot be answered either, because the indices this row has spent are not derivable from it", highWater, err)
	}
	t.Logf("%d of %d attempts refused PERMANENTLY, before and after the corrupt row's bytes were removed. A ratchet stops on attempt 1", permanent, attempts)
}

// AND THE PART OF THE CLASS THAT IS STILL AN UNBOUNDED RETRY, MEASURED RATHER THAN ASSERTED.
//
// This is a RESIDUAL, executable, and it is FILED FOR THE OWNER rather than ruled here. The
// remainder of the ErrStreamStoreState class -- a failed flush, a full disk, an unreadable row
// directory, a CLOSED store -- is still forwarded as transient, and at least one member of it is
// permanent: a closed store answers ErrStreamStoreState on every call for the rest of its life,
// and a ratchet told to retry will ask it for the rest of the process's.
//
// WHY IT IS NOT RULED HERE. Flipping the verdict for the class would wedge a healthy ladder over
// a full disk, which connect/messagegroup's ratchet names as the case that must stay a retry, so
// the trade is a section 8.2 contract question about what the store owes a ratchet rather than an
// implementation choice. The SHAPE of the repair is already demonstrated one case above -- give
// each permanent member its own discriminator in the store, the way the unreadable row just got
// one -- but WHICH members are permanent, and whether a closed store is a caller bug rather than
// a store condition, is the owner's call. SPEC-LEDGER.md lives in a repository this pass must not
// write to, so this case IS the filing: it fails the day the behaviour changes, in either
// direction, and it prints the number it is about.
func TestTheStateClassStillRetriesForeverForTheMembersThatAreNotDiscriminated(t *testing.T) {
	reserver, store := streamAdapterTestReserver(t)
	key, _ := streamAdapterTestKey(t, 0x92)

	if index, err := reserver.Reserve(key); err != nil || index != 1 {
		t.Fatalf("the ladder would not start: (%d, %v)", index, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}

	const attempts = 200
	refused, transient := 0, 0
	var first error
	for attempt := 1; attempt <= attempts; attempt += 1 {
		index, err := reserver.Reserve(key)
		if err == nil {
			t.Fatalf("attempt %d allocated index %d against a closed store", attempt, index)
		}
		refused += 1
		if !errors.Is(err, ErrStreamStoreState) {
			t.Fatalf("attempt %d answered %v, want ErrStreamStoreState underneath", attempt, err)
		}
		if !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
			transient += 1
		}
		if first == nil {
			first = err
		}
	}
	if refused != attempts {
		t.Fatalf("%d of %d attempts were refused", refused, attempts)
	}
	if transient != attempts {
		t.Fatalf(
			"%d of %d refusals against a CLOSED store read as transient and %d read as permanent. This case is the filed residual and it is written against the behaviour as it is: if the ruling for the ErrStreamStoreState class has been changed, change this case with it and say so, because that ruling is what decides whether a full disk wedges a healthy ladder",
			transient, attempts, attempts-transient,
		)
	}
	t.Logf("FILED, NOT RULED: %d of %d refusals against a closed store carry no permanent sentinel, so a SenderRatchet reading them retries without bound. First refusal: %v", transient, attempts, first)
	t.Log("the discriminated member of the same class is the contrast, one case above: a row whose body did not classify at open stops the ladder on attempt 1")
}

// ----------------------------------------------------------------------------------------------
// HOW FAR THIS NOW REACHES, measured rather than asserted.
// ----------------------------------------------------------------------------------------------

// streamAdapterMlsStore is mls.StateStore in a map. It is declared in a _test.go file, so no
// production build of this package can reach it.
type streamAdapterMlsStore struct {
	lock        sync.Mutex
	groupStates map[string][]byte
	privateKeys map[string][]byte
	keyPackages map[string][3][]byte
}

func newStreamAdapterMlsStore() *streamAdapterMlsStore {
	return &streamAdapterMlsStore{
		groupStates: map[string][]byte{},
		privateKeys: map[string][]byte{},
		keyPackages: map[string][3][]byte{},
	}
}

func (self *streamAdapterMlsStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.groupStates[fmt.Sprintf("%x/%d", groupId, epoch)] = append([]byte(nil), state...)
	return nil
}

func (self *streamAdapterMlsStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	state, held := self.groupStates[fmt.Sprintf("%x/%d", groupId, epoch)]
	if !held {
		return nil, fmt.Errorf("no group state for %x at epoch %d", groupId, epoch)
	}
	return append([]byte(nil), state...), nil
}

func (self *streamAdapterMlsStore) DeleteGroupStateBefore(groupId []byte, epoch uint64) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	for at := uint64(0); at < epoch; at += 1 {
		delete(self.groupStates, fmt.Sprintf("%x/%d", groupId, at))
	}
	return nil
}

func (self *streamAdapterMlsStore) PutPrivateKey(pub []byte, priv []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.privateKeys[fmt.Sprintf("%x", pub)] = append([]byte(nil), priv...)
	return nil
}

func (self *streamAdapterMlsStore) GetPrivateKey(pub []byte) ([]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	priv, held := self.privateKeys[fmt.Sprintf("%x", pub)]
	if !held {
		return nil, fmt.Errorf("no private key for %x", pub)
	}
	return append([]byte(nil), priv...), nil
}

func (self *streamAdapterMlsStore) DeletePrivateKey(pub []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	delete(self.privateKeys, fmt.Sprintf("%x", pub))
	return nil
}

func (self *streamAdapterMlsStore) PutKeyPackage(ref []byte, kp []byte, initPriv []byte, encPriv []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.keyPackages[fmt.Sprintf("%x", ref)] = [3][]byte{
		append([]byte(nil), kp...), append([]byte(nil), initPriv...), append([]byte(nil), encPriv...),
	}
	return nil
}

func (self *streamAdapterMlsStore) TakeKeyPackage(ref []byte) ([]byte, []byte, []byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	held, ok := self.keyPackages[fmt.Sprintf("%x", ref)]
	if !ok {
		return nil, nil, nil, fmt.Errorf("no key package for %x", ref)
	}
	delete(self.keyPackages, fmt.Sprintf("%x", ref))
	return held[0], held[1], held[2], nil
}

// streamAdapterRealGroup founds a real one-member MLS group with real keys: a real crypto
// provider, a real signature key pair, a real X-Wing leaf key. The two things standing in are the
// two the fixture in connect/messagegroup also stands in for and names -- the mls state store,
// which is a map, and the clock, which is a constant.
func streamAdapterRealGroup(t *testing.T) messagegroup.GroupHandle {
	t.Helper()
	crypto, err := mls.NewCryptoProvider(mls.CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("the crypto provider: %v", err)
	}
	signer, _, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("the signature key pair: %v", err)
	}
	_, identityPub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("the credential identity: %v", err)
	}
	xwing, err := messagegroup.XwingGenerateKey(bytes.NewReader(crypto.Random(messagegroup.XwingSeedSize)))
	if err != nil {
		t.Fatalf("the x-wing leaf key: %v", err)
	}
	leafKeys, err := (&mls.LeafKeysExtension{
		AlgId:          mls.AlgIdXwing,
		DeviceXwingPub: xwing.Public().Bytes(),
	}).Encode()
	if err != nil {
		t.Fatalf("encode the leaf keys: %v", err)
	}
	engine, err := messagegroup.NewConnectMlsEngine(crypto, newStreamAdapterMlsStore(), signer,
		mls.BasicCredential(identityPub), leafKeys.ExtensionData)
	if err != nil {
		t.Fatalf("the engine: %v", err)
	}
	policy := &mls.GroupPolicyExtension{
		Roles: []mls.RoleEntry{{MemberId: identityPub, Role: mls.RoleOwner}},
	}
	if err := policy.Canonicalize(); err != nil {
		t.Fatalf("canonicalize the policy: %v", err)
	}
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("encode the policy: %v", err)
	}
	groupId := make([]byte, 32)
	copy(groupId, "sdk-stream-adapter-reach")
	handle, err := engine.CreateGroup(groupId, encoded.ExtensionData, leafKeys.ExtensionData)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return handle
}

// THE FIRST PRODUCTION PATH FROM A GroupSession TO A DURABLE RESERVATION, and this case is the
// measurement of how far it reaches.
//
// Everything on the key path is real: a real MLS group, a real epoch exporter, real epoch read and
// write keys, a real record key ladder, and a real *sdk.StreamStore on a real directory reached
// through the production adapter this task produces. The stand-ins are named rather than left to
// be found -- the mls state store is a map, the clock is a constant, pq_secret is drawn by the
// production NewPqSecret but is not delivered by anything, and server_nonce is a constant because
// there is no connection to have chosen one.
//
// WHAT IT PROVES: the reservation the seal rests on is ON DISK before the record exists, and it
// survives the process that made it -- a fresh store over the same directory answers the same
// high water. WHAT IT DOES NOT PROVE is in the log line at the end.
func TestAGroupSessionSealsADurableRecordOverTheProductionStore(t *testing.T) {
	handle := streamAdapterRealGroup(t)
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	reserver := NewStreamIndexReserver(store)

	pqSecret, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("NewPqSecret: %v", err)
	}
	serverNonce := []byte("sdk-stream-adapter-has-no-connection")
	session, err := messagegroup.NewGroupSession(handle, pqSecret, nil, reserver,
		func() int64 { return 1_700_000_000_000 }, serverNonce)
	if err != nil {
		t.Fatalf("NewGroupSession over the production reserver: %v", err)
	}
	defer session.Close()

	senderHandle, err := session.SenderHandle()
	if err != nil {
		t.Fatalf("SenderHandle: %v", err)
	}
	key, err := streamKeyFromOctets(handle.GroupId(), senderHandle[:])
	if err != nil {
		t.Fatalf("the stream this session will allocate on: %v", err)
	}
	rowName := streamRowName(key)
	path := filepath.Join(store.rowDir, rowName)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the row exists before the seal: %v", err)
	}

	// THE DURABILITY IS READ AT THE MOMENT THE SEAL RETURNS, not afterwards. A row that is on
	// disk by the time a later Stat runs says nothing about whether the reservation was forced
	// down before the record that rests on it existed; the store's forced-flush counter is
	// taken AFTER Sync returns -- see forceFlush -- so an increment across this call is the
	// flush having been PERFORMED inside it.
	flushesBefore := store.rowFlushCount()
	record, err := session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("a real durable record"), 0, nil)
	flushesAfter := store.rowFlushCount()
	if err != nil {
		t.Fatalf("SealRecord over a durable reserver: %v", err)
	}
	if record == nil {
		t.Fatal("SealRecord answered no record and no error")
	}
	if record.Header.StreamIndex != 1 {
		t.Errorf("the sealed record carries stream_index %d, want 1", record.Header.StreamIndex)
	}
	if len(record.CtBody) == 0 || record.WriteAuth == ([32]byte{}) {
		t.Error("the record carries no ciphertext or no write_auth")
	}

	if flushesAfter <= flushesBefore {
		t.Errorf(
			"the seal returned with %d forced flush(es) counted and %d before it: the reservation this record's stream_index rests on had not been forced to disk when the record came into existence, so a crash here leaves a sealed record whose index no row records",
			flushesAfter, flushesBefore,
		)
	}

	// the reservation is DURABLE and it was durable before the record existed.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the row the seal reserved on: %v", err)
	}
	if int64(len(body)) != streamRecordWidth {
		t.Errorf("the row is %d octets after one seal, want %d", len(body), streamRecordWidth)
	}
	if index := streamRecordIndex(body); index != record.Header.StreamIndex {
		t.Errorf("the row records index %d and the record carries stream_index %d", index, record.Header.StreamIndex)
	}
	if !verifyStreamRecord(rowName, body) {
		t.Error("the row's record does not verify under its own name")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}
	reopened := streamTestOpen(t, dir)
	if highWater, err := NewStreamIndexReserver(reopened).HighWater(key); err != nil || highWater != 1 {
		t.Errorf("a fresh store over the same directory answered (%d, %v), want (1, nil)", highWater, err)
	}

	keys, err := session.EpochKeys()
	if err != nil {
		t.Fatalf("EpochKeys: %v", err)
	}
	epoch, err := keys.Epoch()
	if err != nil {
		t.Fatalf("the epoch these keys were expanded at: %v", err)
	}
	writeKey, err := keys.WriteKey()
	if err != nil {
		t.Fatalf("the epoch write key: %v", err)
	}
	if len(writeKey) == 0 {
		t.Error("the epoch write key is empty")
	}
	t.Logf("REACHED: a real MLS group at epoch %d, a real epoch write key of %d octets, a real record key ladder, and a DURABLE reservation at index %d on disk at %s",
		epoch, len(writeKey), record.Header.StreamIndex, path)
	t.Logf("THE RESERVATION WAS DURABLE BEFORE THE SEAL RETURNED: %d forced flush(es) before the call, %d after, and the row carried the index the record carries",
		flushesBefore, flushesAfter)
	t.Logf("WHAT STANDS BETWEEN THIS RECORD AND msgrepo's Submit, measured on this tree:")
	t.Logf("  1. THE PROJECTION. api/submit.go takes a *protocol.SubmitRequest whose records are *protocol.Record, and re-projects ParseRecord(record_bytes) itself to compare with proto.Equal. Package sdk has ZERO production references to protocol.Record or protocol.SubmitRequest, so this sealed *message.Record has no wire form at all. This is the first missing piece and it is the plan's Task 8")
	t.Logf("  2. THE TRANSPORT. There is no connect.Client binding in sdk at any section 10.1 code point, no request_id correlation and no section 4.6 fragmentation, so there is nothing to carry a request even once one exists")
	t.Logf("  3. THE SERVER NONCE. write_auth is a mac over the submitting connection's nonce. This case supplies a constant because there is no connection; Hello is what supplies a real one, and GroupSession.RebindServerNonce -- which exists and has zero production call sites anywhere in these three trees -- is what installs it. A record sealed under a constant nonce is refused by check 7")
	t.Log("  and none of the three is built here")
}
