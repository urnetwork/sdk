//go:build !sdk_mobile_bind

package sdk

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/messagegroup"
)

// ----------------------------------------------------------------------------------------------
// fixtures
// ----------------------------------------------------------------------------------------------

// streamKeyPreA1 is the key wave 1 shipped, BEFORE ruling A1 removed the retention-class byte.
// It exists here and only here, so a test can write what a pre-A1 build would have written by
// handing the production derivation the pre-A1 FIELD SET rather than by reimplementing that
// derivation. Nothing in production may name these fields; see
// TestNoProductionSourceOfPackageSdkSpellsAStreamKeyFieldName.
type streamKeyPreA1 struct {
	GroupId       [32]byte
	SenderHandle  [16]byte
	RetentionWire byte
}

func streamTestOctets(width int, seed byte) []byte {
	octets := make([]byte, width)
	for i := range octets {
		octets[i] = seed + byte(i)
	}
	return octets
}

func streamTestKeyOctets(t *testing.T, seed byte) [][]byte {
	t.Helper()
	keyType := streamKeyType()
	parts := make([][]byte, 0, keyType.NumField())
	for i := range keyType.NumField() {
		parts = append(parts, streamTestOctets(keyType.Field(i).Type.Len(), seed+byte(17*i)))
	}
	return parts
}

func streamTestRowName(t *testing.T, parts [][]byte) string {
	t.Helper()
	key, err := streamKeyFromOctets(parts...)
	if err != nil {
		t.Fatalf("the fixture's own key would not flatten: %v", err)
	}
	return streamRowName(key)
}

// streamTestRowBody builds a row carrying one whole verifying record per index, in order.
func streamTestRowBody(rowName string, indices ...uint64) []byte {
	body := make([]byte, 0, len(indices)*streamRecordWidth)
	for _, index := range indices {
		record := encodeStreamRecord(rowName, index)
		body = append(body, record[:]...)
	}
	return body
}

// streamTestCorruptRecord overwrites one whole record's octets IN PLACE, leaving the row's
// length unchanged, so that record fails its checksum.
func streamTestCorruptRecord(body []byte, position int) {
	for i := (position - 1) * streamRecordWidth; i < position*streamRecordWidth; i += 1 {
		body[i] ^= 0xff
	}
}

func streamTestPlantRow(t *testing.T, dir string, rowName string, body []byte) string {
	t.Helper()
	rowDir := filepath.Join(dir, streamRowDirName)
	if err := os.MkdirAll(rowDir, 0o700); err != nil {
		t.Fatalf("plant the row directory: %v", err)
	}
	path := filepath.Join(rowDir, rowName)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("plant row %s: %v", rowName, err)
	}
	return path
}

// streamTestAppendOneRecord is the ONE append an allocation performs, simulated here because the
// allocation path -- ReserveStreamIndex, its fsync boundary and its two sentinels -- is the next
// task's and does not exist yet. It uses the production encoder, appends exactly one whole
// record at EOF and flushes, which is the discipline classifyStreamRow's case-2 bound is derived
// from.
func streamTestAppendOneRecord(t *testing.T, path string, rowName string, index uint64) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open %s to append: %v", path, err)
	}
	defer file.Close()
	record := encodeStreamRecord(rowName, index)
	if _, err := file.Write(record[:]); err != nil {
		t.Fatalf("append a record to %s: %v", path, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("flush %s: %v", path, err)
	}
}

func streamTestRowLength(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func streamTestOpen(t *testing.T, dir string) *StreamStore {
	t.Helper()
	store, err := OpenStreamStore(dir)
	if err != nil {
		t.Fatalf("open the store at %s: %v", dir, err)
	}
	t.Cleanup(func() {
		store.Close()
	})
	return store
}

// ----------------------------------------------------------------------------------------------
// Property 1 -- a row is identified by exactly the fields the RESERVATION is keyed by, and the
// identity is DERIVED from StreamKey's own field set rather than spelled.
// ----------------------------------------------------------------------------------------------

// CLASS: the fields of connect/messagegroup.StreamKey.
// SCOPE, derived separately: the type StreamKey as connect/messagegroup declares it TODAY, read
// through reflection at test time -- not a copy of its field names kept in sdk. A gate that
// listed GroupId and SenderHandle would survive the day a third field returns and is therefore
// not this gate. The gate REPORTS the number of fields it read, so a field added or removed in
// connect fails here rather than silently re-keying every row.
func TestStreamRowIdentityIsDerivedFromStreamKeysOwnFieldSet(t *testing.T) {
	keyType := streamKeyType()
	fieldCount := keyType.NumField()
	t.Logf("CLASS: %d field(s) of %s", fieldCount, keyType.String())
	t.Logf(
		"SCOPE: the 1 type connect/messagegroup declares today, read through reflection at test time; 0 lists of field names kept in sdk were read",
	)
	if fieldCount == 0 {
		t.Fatalf("%s declares no fields, so this gate read nothing", keyType.String())
	}

	// the key-space tag the store carries is the tag of the type connect declares, and not of
	// anything sdk keeps a copy of.
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	wantTag := streamKeySpaceTagOf(keyType)
	if store.keySpaceTag != wantTag {
		t.Errorf(
			"the store's key-space tag is %q and the tag of %s (%d fields, read by reflection here) is %q; the store is keying rows off some other field set",
			store.keySpaceTag,
			keyType.String(),
			fieldCount,
			wantTag,
		)
	}

	// the row name is TAG then IDENTITY, fixed width, tag first.
	parts := streamTestKeyOctets(t, 1)
	rowName := streamTestRowName(t, parts)
	if len(rowName) != streamRowNameLen {
		t.Errorf("a row name is %d characters, want %d (a %d-character tag then a %d-character identity)",
			len(rowName), streamRowNameLen, streamKeySpaceTagLen, streamRowIdentityLen)
	}
	if !strings.HasPrefix(rowName, wantTag) {
		t.Errorf(
			"the row name %q does not begin with the key-space tag %q; a tag that is not a fixed-width prefix cannot be read off a foreign row's name, and a foreign row that cannot be read is an absent row answered (0, nil)",
			rowName,
			wantTag,
		)
	}

	// EVERY field separates a row, derived off the type rather than written as the cases this
	// type happens to have today.
	for i := range fieldCount {
		field := keyType.Field(i)
		base := reflect.New(keyType)
		apart := reflect.New(keyType)
		differing := apart.Elem().Field(i)
		if differing.Kind() != reflect.Array {
			differing.SetUint(1)
		} else {
			differing.Index(0).SetUint(1)
		}
		if base.Elem().Interface() == apart.Elem().Interface() {
			t.Fatalf("%s.%s could not be made to differ, so this case cannot judge it", keyType.String(), field.Name)
		}
		if streamRowNameOf(base.Elem()) == streamRowNameOf(apart.Elem()) {
			t.Errorf(
				"two keys differing only in %s.%s derive the SAME row name, so the two streams share one row and the second is handed indices the first has already used",
				keyType.String(),
				field.Name,
			)
		}
	}

	// the flattening takes exactly as many positional parameters as the type declares fields.
	if _, err := streamKeyFromOctets(parts...); err != nil {
		t.Errorf("the flattening refused %d well-formed key parameters: %v", fieldCount, err)
	}
	for _, wrong := range []int{fieldCount - 1, fieldCount + 1} {
		if wrong < 0 {
			continue
		}
		offered := make([][]byte, 0, wrong)
		for i := range wrong {
			width := 32
			if i < fieldCount {
				width = keyType.Field(i).Type.Len()
			}
			offered = append(offered, streamTestOctets(width, 9))
		}
		_, err := streamKeyFromOctets(offered...)
		if !errors.Is(err, ErrStreamKeyWidth) {
			t.Errorf(
				"the flattening answered %v for %d key parameters when %s declares %d fields; a field added or removed in connect must arrive here as a refusal and not as a zero-valued field nobody passed",
				err,
				wrong,
				keyType.String(),
				fieldCount,
			)
		}
	}
}

// The other half of Property 1, and the half that is the ledger-21 defect if it is missing: the
// derivation must be a DERIVATION and not two spelled names that happen to agree with it today.
//
// CLASS: the field names of connect/messagegroup.StreamKey, read through reflection at test
// time. Not a list.
// SCOPE, derived separately: every non-test .go file in package sdk's own directory. The
// narrowing this gate performs is production-versus-test, and its complement -- the test files,
// which MAY spell a field name because a test is where a pre-A1 fixture is built -- is printed
// below with its size, together with the assertion that scope and complement partition the
// directory's .go files exactly.
// Comments are NOT read: a comment cannot key a row, and the package doc in message.go names
// both fields deliberately.
func TestNoProductionSourceOfPackageSdkSpellsAStreamKeyFieldName(t *testing.T) {
	keyType := streamKeyType()
	names := map[string]bool{}
	for i := range keyType.NumField() {
		names[keyType.Field(i).Name] = true
	}
	t.Logf("CLASS: %d field name(s) of %s, read through reflection at test time: %v", len(names), keyType.String(), slices.Sorted(maps.Keys(names)))
	if len(names) == 0 {
		t.Fatalf("%s declares no fields, so this gate looked for nothing", keyType.String())
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	all := []string{}
	scope := []string{}
	complement := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		all = append(all, entry.Name())
		if strings.HasSuffix(entry.Name(), "_test.go") {
			complement = append(complement, entry.Name())
		} else {
			scope = append(scope, entry.Name())
		}
	}
	t.Logf("SCOPE: %d production .go file(s) in package sdk's own directory", len(scope))
	t.Logf("COMPLEMENT removed by this narrowing: %d test .go file(s): %v", len(complement), complement)
	if len(scope) == 0 {
		t.Fatal("the scope is empty, so this gate read no production source at all")
	}
	if len(complement) == 0 {
		t.Fatal("the complement is empty, which means the production/test narrowing removed nothing and this gate is not the gate it says it is")
	}
	if len(all) != len(scope)+len(complement) {
		t.Fatalf("the narrowing does not partition the directory: %d .go files, %d in scope, %d in the complement", len(all), len(scope), len(complement))
	}

	fileSet := token.NewFileSet()
	sawTheKeyType := []string{}
	for _, name := range scope {
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err == nil && path == "github.com/urnetwork/connect/messagegroup" {
				sawTheKeyType = append(sawTheKeyType, name)
			}
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.Ident:
				if names[typed.Name] {
					t.Errorf(
						"%s spells the identifier %s, which is a field name of %s read by reflection in this test; the row derivation must read the field set rather than name it, or a field added or removed in connect silently re-keys every row instead of failing here",
						fileSet.Position(typed.Pos()),
						typed.Name,
						keyType.String(),
					)
				}
			case *ast.BasicLit:
				if typed.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(typed.Value)
				if err == nil && names[value] {
					t.Errorf(
						"%s carries the string literal %q, which is a field name of %s read by reflection in this test",
						fileSet.Position(typed.Pos()),
						value,
						keyType.String(),
					)
				}
			}
			return true
		})
	}
	if len(sawTheKeyType) == 0 {
		t.Errorf(
			"no production file in the scope imports connect/messagegroup, so this gate read no file in which a %s field could appear at all",
			keyType.String(),
		)
	} else {
		t.Logf("production files that can name a %s: %v", keyType.String(), sawTheKeyType)
	}
}

// ----------------------------------------------------------------------------------------------
// Property 2 -- a row whose key space this build did not produce is REFUSED, never answered zero.
// ----------------------------------------------------------------------------------------------

// This is ledger item 170, executable. A store holding rows under the pre-A1 three-field key
// answers HighWater 0 for an A1 key unless something refuses; the ladder then resumes at 1 and
// re-issues record_key[1] under a class key that has not moved, which is a repeated (key, nonce)
// on BOTH of a record's AEADs.
//
// CLASS: the three partitions of a name in the row directory -- (a) this build's tag, (b) a
// fixed-width tag that is not this build's whatever its value, (c) anything else.
// SCOPE, derived separately: the ROW DIRECTORY's entries, enumerated, and not the single path a
// key names. A gate written over one stat cannot see (b) at all.
func TestARowOfAnotherKeySpaceIsRefusedAndNeverAnsweredZero(t *testing.T) {
	parts := streamTestKeyOctets(t, 3)

	preA1 := streamKeyPreA1{}
	copy(preA1.GroupId[:], parts[0])
	copy(preA1.SenderHandle[:], parts[1])
	preA1.RetentionWire = 1
	foreignName := streamRowNameOf(reflect.ValueOf(preA1))
	thisTag := streamKeySpaceTagOf(streamKeyType())

	if len(foreignName) != streamRowNameLen {
		t.Fatalf(
			"the pre-A1 row name is %d characters and this build's is %d; if a foreign row does not parse as tag-then-identity it falls into partition (c) and would be owed an error the store does not produce for it",
			len(foreignName),
			streamRowNameLen,
		)
	}
	if strings.HasPrefix(foreignName, thisTag) {
		t.Fatalf("the pre-A1 field set derives this build's key-space tag %q, so the fixture cannot plant a foreign row at all", thisTag)
	}
	t.Logf("this build's key-space tag is %q; the pre-A1 (three-field) tag is %q", thisTag, foreignName[:streamKeySpaceTagLen])
	exercised := map[streamRowClass]int{}
	planted := 0

	dir := t.TempDir()
	streamTestPlantRow(t, dir, foreignName, streamTestRowBody(foreignName, 1, 2, 3))
	store := streamTestOpen(t, dir)
	planted += 1

	highWater, err := store.StreamHighWater(parts[0], parts[1])
	if !errors.Is(err, ErrStreamKeySpace) {
		t.Errorf(
			"a row written under the pre-A1 key derivation was answered (%d, %v); want ErrStreamKeySpace. A (0, nil) here is ledger item 170 reproduced in sdk: the ladder restarts at index 1 under a class key that has not moved, which is a reused nonce under a reused record_key",
			highWater,
			err,
		)
	}
	if errors.Is(err, ErrStreamStoreState) {
		t.Errorf(
			"a foreign-key-space row was answered ErrStreamStoreState (%v); partition (b) is a row this build cannot KEY, not an entry that is not a row, and collapsing the two loses the only refusal that names the transition rule",
			err,
		)
	}
	if highWater != 0 {
		t.Errorf("the refusal carried a high water of %d; a refusal answers no index", highWater)
	}

	// partition (c): an entry in the row directory that is not a row under any tag.
	exercised[store.classifyStreamRowName(foreignName)] += 1
	exercised[store.classifyStreamRowName(streamTestRowName(t, parts))] += 1
	for _, notARow := range []string{
		"notarow",
		strings.Repeat("z", streamRowNameLen),
		strings.Repeat("a", streamRowNameLen-1),
	} {
		exercised[store.classifyStreamRowName(notARow)] += 1
		other := t.TempDir()
		streamTestPlantRow(t, other, notARow, []byte("x"))
		otherStore := streamTestOpen(t, other)
		planted += 1
		_, err := otherStore.StreamHighWater(parts[0], parts[1])
		if !errors.Is(err, ErrStreamStoreState) {
			t.Errorf("%q in the row directory was answered %v; an entry that is not a row under any tag is ErrStreamStoreState", notARow, err)
		}
		if errors.Is(err, ErrStreamKeySpace) {
			t.Errorf("%q in the row directory was answered ErrStreamKeySpace; it parses as no tag at all", notARow)
		}
	}

	// and a non-regular entry: the row directory holds rows and nothing else, by
	// construction, so the exclusion the next task adds sits beside it and never in it.
	nested := t.TempDir()
	if err := os.MkdirAll(filepath.Join(nested, streamRowDirName, "subdir"), 0o700); err != nil {
		t.Fatalf("plant a directory inside the row directory: %v", err)
	}
	nestedStore := streamTestOpen(t, nested)
	planted += 1
	if _, err := nestedStore.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("a directory inside the row directory was answered %v; want ErrStreamStoreState", err)
	}

	// and the case the name cannot catch: a DIRECTORY whose name parses as an ordinary row
	// of some OTHER key. Nothing about the name is wrong, so only "the row directory holds
	// regular files and nothing else" refuses it -- and without that refusal the entry is
	// skipped and the answer is the silent (0, nil) this whole property exists to prevent.
	otherParts := streamTestKeyOctets(t, 71)
	shaped := streamTestRowName(t, otherParts)
	if shaped == streamTestRowName(t, parts) {
		t.Fatal("the two fixture keys derive one row name, so this case cannot judge anything")
	}
	shapedDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shapedDir, streamRowDirName, shaped), 0o700); err != nil {
		t.Fatalf("plant a row-shaped directory: %v", err)
	}
	shapedStore := streamTestOpen(t, shapedDir)
	planted += 1
	exercised[shapedStore.classifyStreamRowName(shaped)] += 1
	exercised[shapedStore.classifyStreamRowName("subdir")] += 1
	t.Logf(
		"CLASS: the 3 partitions a name in the row directory falls into -- (a) this build's tag, (b) a fixed-width tag that is not this build's whatever its value, (c) anything else -- exercised %d(a) / %d(b) / %d(c)",
		exercised[streamRowOfThisKeySpace],
		exercised[streamRowOfAnotherKeySpace],
		exercised[streamRowNotARow],
	)
	t.Logf(
		"SCOPE: the row directory's entries, ENUMERATED -- a gate written over the single path a key names cannot see partition (b) at all -- over %d planted store(s)",
		planted,
	)
	for partition, name := range map[streamRowClass]string{
		streamRowOfThisKeySpace:    "(a)",
		streamRowOfAnotherKeySpace: "(b)",
		streamRowNotARow:           "(c)",
	} {
		if exercised[partition] == 0 {
			t.Errorf("partition %s was exercised by no entry at all, so this gate judged only part of the class it derived", name)
		}
	}
	if highWater, err := shapedStore.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreState) {
		t.Errorf(
			"a directory named %q in the row directory was answered (%d, %v); it is not a row, and skipping it answers a stream that may well have been written to with the error-free zero contract clause 4 reserves for a stream never seen",
			shaped,
			highWater,
			err,
		)
	}
}

// ----------------------------------------------------------------------------------------------
// Property 3 -- a key of the wrong width is refused at the boundary, before anything derives
// from it.
// ----------------------------------------------------------------------------------------------

// CLASS: every field of StreamKey, and for each of them every width that is not the one the
// field holds -- derived from the field's own array length, never from the numbers 32 and 16.
// SCOPE, derived separately: the flattening, which is the single place in package sdk where a
// []byte becomes a StreamKey, reached here through the store's own read path so a store that
// checked the width somewhere other than the boundary is still caught.
//
// This is not hygiene. messagegroup.GroupHandleKey and messagegroup.SenderHandle PANIC on a
// wrong-width input, defended by the argument that nothing there is reachable from the network.
// A durable store is what makes such a value a row read off a disk. S2-8.
func TestAKeyOfTheWrongWidthIsRefusedAtTheBoundary(t *testing.T) {
	keyType := streamKeyType()
	if keyType.NumField() != 2 {
		t.Fatalf(
			"%s declares %d fields and section 8.2's read path takes two positional parameters; the arity half of this is TestStreamRowIdentityIsDerivedFromStreamKeysOwnFieldSet, and this case cannot be driven through the store until the read path's own arity moves with it",
			keyType.String(),
			keyType.NumField(),
		)
	}
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	good := streamTestKeyOctets(t, 5)
	refusalsExercised := 0

	if _, err := store.StreamHighWater(good[0], good[1]); err != nil {
		t.Fatalf("well-formed key octets were refused: %v", err)
	}

	for i := range keyType.NumField() {
		field := keyType.Field(i)
		want := field.Type.Len()
		for _, offeredWidth := range []int{0, want - 1, want + 1, 2 * want} {
			if offeredWidth == want {
				continue
			}
			offered := make([][]byte, len(good))
			copy(offered, good)
			offered[i] = streamTestOctets(offeredWidth, 7)
			refusalsExercised += 1
			_, err := store.StreamHighWater(offered[0], offered[1])
			if !errors.Is(err, ErrStreamKeyWidth) {
				t.Errorf(
					"a %d-octet value offered for %s.%s (which holds %d) was answered %v; a short key silently padded or a long key silently truncated collides two streams onto one row, and the second stream is then handed indices the first has already used",
					offeredWidth,
					keyType.String(),
					field.Name,
					want,
					err,
				)
				continue
			}
			message := err.Error()
			for _, owed := range []string{
				field.Name,
				fmt.Sprintf("%d", offeredWidth),
				fmt.Sprintf("%d", want),
			} {
				if !strings.Contains(message, owed) {
					t.Errorf("the width refusal %q does not name %q; the refusal owes which parameter it refused and what width it had", message, owed)
				}
			}
		}
		// nil is not a width of zero by accident: it must take the same refusal.
		offered := make([][]byte, len(good))
		copy(offered, good)
		offered[i] = nil
		refusalsExercised += 1
		if _, err := store.StreamHighWater(offered[0], offered[1]); !errors.Is(err, ErrStreamKeyWidth) {
			t.Errorf("a nil value offered for %s.%s was answered %v; want ErrStreamKeyWidth", keyType.String(), field.Name, err)
		}
	}
	t.Logf(
		"CLASS: %d wrong-width value(s), derived as every field's own array length plus and minus one, zero, doubled, and nil, over the %d field(s) of %s",
		refusalsExercised,
		keyType.NumField(),
		keyType.String(),
	)
	t.Logf("SCOPE: the 1 flattening in package sdk, reached through the store's own read path rather than called directly")
	if refusalsExercised == 0 {
		t.Fatal("this gate exercised no refusal at all, which is a gate that read nothing rather than a clean one")
	}
}

// ----------------------------------------------------------------------------------------------
// Property 4 -- a present-but-unreadable row is an error, it is a DIFFERENT error from an absent
// one, and a TORN TAIL is neither.
// ----------------------------------------------------------------------------------------------

// CLASS: the three cases of the decision procedure in classifyStreamRow, and the discriminator
// between them is POSITION and SIZE, never content -- derived here from the record width the
// format declares, so a change to the width moves every fixture with it.
// SCOPE, derived separately: a row's bytes as they lie on disk, read back through the store's
// own read path after a real OpenStreamStore, so the open-time repair is inside the scope rather
// than beside it.
//
// The two NUMBERS this property reports, because the truncation-versus-skip position is
// invisible to every assertion that reads only an answer: the row writes OpenStreamStore
// performs (0 when it found no torn tail, 1 when it found one) and the row writes the read path
// performs (always 0).
func TestARowsThreeCasesAndTheDiscriminatorBetweenThem(t *testing.T) {
	parts := streamTestKeyOctets(t, 11)
	rowName := streamTestRowName(t, parts)
	half := streamRecordWidth / 2

	intact := streamTestRowBody(rowName, 1, 2, 3)

	tornPartial := append(streamTestRowBody(rowName, 1, 2), streamTestRowBody(rowName, 3)[:half]...)

	tornWhole := streamTestRowBody(rowName, 1, 2, 3)
	streamTestCorruptRecord(tornWhole, 3)

	noVerifyingRecord := streamTestRowBody(rowName, 1)
	streamTestCorruptRecord(noVerifyingRecord, 1)

	twoFailingWhole := streamTestRowBody(rowName, 1, 2, 3)
	streamTestCorruptRecord(twoFailingWhole, 2)
	streamTestCorruptRecord(twoFailingWhole, 3)

	failingWithVerifyingAfter := streamTestRowBody(rowName, 1, 2, 3)
	streamTestCorruptRecord(failingWithVerifyingAfter, 2)

	cutToHalf := streamTestRowBody(rowName, 1, 2, 3)[:3*streamRecordWidth/2]

	oneFailingWholeThenPartial := append(
		append(streamTestRowBody(rowName, 1), streamTestRowBody(rowName, 2)[:half]...),
		streamTestRowBody(rowName, 2)...,
	)

	cases := map[string]int{}
	for _, testCase := range []struct {
		name string
		// plant is nil for an absent row.
		plant []byte
		// wantPresent says whether a row file should exist at all.
		wantPresent   bool
		wantHighWater uint64
		wantErr       error
		// wantShape is the shape of case 3 the refusal owes by name. Two of case 3's
		// shapes carry the SAME error value, so a gate that reads only errors.Is cannot
		// tell "a failing record with a verifying record after it" from "a failing suffix
		// of two whole records" -- and a store that stopped looking for the verifying
		// record would answer the second for the first and go unnoticed.
		wantShape         string
		wantLength        int64
		wantOpenRowWrites int
		why               string
	}{
		{
			name:        "case 1: an absent row",
			wantPresent: false,
			wantErr:     nil,
			why:         "contract clause 4: a stream never seen is 0 with no error, so the first allocation is 1",
		},
		{
			name:              "case 2: a present zero-length row",
			plant:             []byte{},
			wantPresent:       true,
			wantErr:           nil,
			wantLength:        0,
			wantOpenRowWrites: 0,
			why:               "a row carrying no verifying record is the state a row is in before an index for its key has been handed out, which is the state a stream never seen is in",
		},
		{
			name:              "an intact row",
			plant:             intact,
			wantPresent:       true,
			wantHighWater:     3,
			wantErr:           nil,
			wantLength:        3 * streamRecordWidth,
			wantOpenRowWrites: 0,
			why:               "every record verifies and the length is a whole multiple of the record width, so nothing is owed and nothing is written",
		},
		{
			name:              "case 2: a trailing partial with every whole record verifying",
			plant:             tornPartial,
			wantPresent:       true,
			wantHighWater:     2,
			wantErr:           nil,
			wantLength:        2 * streamRecordWidth,
			wantOpenRowWrites: 1,
			why:               "an interrupted append leaves a partial; refusing it would leave a row no later process could open, on exactly the path the durability exists to survive",
		},
		{
			name:              "case 2: a final whole record torn within its own octets",
			plant:             tornWhole,
			wantPresent:       true,
			wantHighWater:     2,
			wantErr:           nil,
			wantLength:        2 * streamRecordWidth,
			wantOpenRowWrites: 1,
			why:               "the second of the two shapes one interrupted append can leave",
		},
		{
			name:              "case 2: no verifying record at all",
			plant:             noVerifyingRecord,
			wantPresent:       true,
			wantHighWater:     0,
			wantErr:           nil,
			wantLength:        0,
			wantOpenRowWrites: 1,
			why:               "f == 1, so the answer is (0, nil) and the row is truncated to nothing",
		},
		{
			name:              "case 2: a three-record row cut to half its length",
			plant:             cutToHalf,
			wantPresent:       true,
			wantHighWater:     1,
			wantErr:           nil,
			wantLength:        streamRecordWidth,
			wantOpenRowWrites: 1,
			why:               "a truncated row and an interrupted append are the SAME BYTES; no function of the length, the width and the checksum verdicts can answer them differently, so both are case 2 and a store that refuses this wedges every open after any crash mid-append",
		},
		{
			name:              "case 3: a failing suffix of two whole records",
			plant:             twoFailingWhole,
			wantPresent:       true,
			wantErr:           ErrStreamStoreState,
			wantShape:         "failing suffix of 2 whole records",
			wantLength:        3 * streamRecordWidth,
			wantOpenRowWrites: 0,
			why:               "one interrupted append can damage exactly one record, so two is corruption; answering the last verifying record here is an index handed out twice",
		},
		{
			name:              "case 3: a failing record with a verifying record after it",
			plant:             failingWithVerifyingAfter,
			wantPresent:       true,
			wantErr:           ErrStreamStoreState,
			wantShape:         "a verifying record at position 3",
			wantLength:        3 * streamRecordWidth,
			wantOpenRowWrites: 0,
			why:               "a failure with a verifying record after it is a corrupt body however small it is",
		},
		{
			name:              "case 3: one failing whole record with a partial after it",
			plant:             oneFailingWholeThenPartial,
			wantPresent:       true,
			wantErr:           ErrStreamStoreState,
			wantShape:         "partial after it",
			wantLength:        int64(len(oneFailingWholeThenPartial)),
			wantOpenRowWrites: 0,
			why:               "two records' worth of damage, which no single interrupted append produces; admitting it to case 2 answers R1 after every restart, which is one stream_index handed out for the life of the row",
		},
	} {
		cases[strings.SplitN(testCase.name, ":", 2)[0]] += 1
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, streamRowDirName, rowName)
			if testCase.wantPresent {
				streamTestPlantRow(t, dir, rowName, testCase.plant)
			}
			store := streamTestOpen(t, dir)
			openRowWrites := store.rowWriteCount()
			t.Logf("OpenStreamStore performed %d row write(s)", openRowWrites)
			if openRowWrites != testCase.wantOpenRowWrites {
				t.Errorf(
					"OpenStreamStore performed %d row write(s), want %d. %s",
					openRowWrites,
					testCase.wantOpenRowWrites,
					testCase.why,
				)
			}
			if testCase.wantPresent {
				if got := streamTestRowLength(t, path); got != testCase.wantLength {
					t.Errorf(
						"after OpenStreamStore the row is %d octets, want %d; the discard is a truncation performed by the writer at open, so that the row's length is a whole multiple of %d at every moment an append begins",
						got,
						testCase.wantLength,
						streamRecordWidth,
					)
				}
			}

			before := store.rowWriteCount()
			highWater, err := store.StreamHighWater(parts[0], parts[1])
			after := store.rowWriteCount()
			t.Logf("the read path performed %d row write(s)", after-before)
			if after != before {
				t.Errorf(
					"the read path performed %d row write(s), want 0; a repair done lazily in the read path answers every question this property asks correctly and turns a read into a second writer to one row inside one store",
					after-before,
				)
			}

			if testCase.wantErr == nil {
				if err != nil {
					t.Fatalf("answered %v, want (%d, nil). %s", err, testCase.wantHighWater, testCase.why)
				}
				if highWater != testCase.wantHighWater {
					t.Errorf("answered high water %d, want %d. %s", highWater, testCase.wantHighWater, testCase.why)
				}
				return
			}
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("answered (%d, %v), want %v. %s", highWater, err, testCase.wantErr, testCase.why)
			}
			if testCase.wantShape != "" && !strings.Contains(err.Error(), testCase.wantShape) {
				t.Errorf(
					"the refusal %q does not name the shape it found (%q); case 3's shapes share one error value, so a refusal that does not name its shape cannot tell them apart and a store that stopped discriminating between them would answer one for another unnoticed",
					err.Error(),
					testCase.wantShape,
				)
			}
			if highWater != 0 {
				t.Errorf(
					"the refusal carried a high water of %d; a refusal answers no index, and answering the last surviving record's value here is exactly the index reuse the refusal exists to prevent",
					highWater,
				)
			}
		})
	}
	total := 0
	for _, count := range cases {
		total += count
	}
	t.Logf("CLASS: %d row fixture(s) over the decision procedure's cases: %v", total, cases)
	t.Logf(
		"SCOPE: a row's bytes as they lie on disk, read back through %d real OpenStreamStore call(s) and %d read-path call(s), so the open-time repair is inside the scope rather than beside it",
		total,
		total,
	)
	if total == 0 {
		t.Fatal("this gate walked no fixture at all, which is a gate that read nothing rather than a clean one")
	}
}

// The half of Property 4 that no answer can see: WHERE THE NEXT APPEND LANDS.
//
// Plant R1 then half of R2 at L = 1.5W, open, allocate once, close, reopen. Under a store that
// SKIPS the torn octets instead of truncating them, the first open writes nothing and the row
// becomes R1, half-R2, R2' at L = 2.5W -- and the second open sees k = 2 with R_2 spanning
// half-R2 and the head of R2', which is one failing whole record with a partial after it. Under
// the bound this store used to read, that answers R1 AGAIN, after every restart, for the life of
// the row: one stream_index handed out twice, which spec A section 5.6 calls "a total break of
// both AEADs for that record".
func TestATornTailIsTruncatedBeforeTheNextAppendLands(t *testing.T) {
	parts := streamTestKeyOctets(t, 23)
	rowName := streamTestRowName(t, parts)
	half := streamRecordWidth / 2

	dir := t.TempDir()
	torn := append(streamTestRowBody(rowName, 1), streamTestRowBody(rowName, 2)[:half]...)
	path := streamTestPlantRow(t, dir, rowName, torn)
	if got := streamTestRowLength(t, path); got != int64(streamRecordWidth+half) {
		t.Fatalf("the fixture is %d octets, want %d (one whole record and half of another)", got, streamRecordWidth+half)
	}

	store := streamTestOpen(t, dir)
	if got := store.rowWriteCount(); got != 1 {
		t.Errorf(
			"OpenStreamStore performed %d row write(s) on a row with a torn tail, want 1; a store that leaves the torn octets on disk and appends at EOF hands out the same index after every restart",
			got,
		)
	}
	if got := streamTestRowLength(t, path); got != streamRecordWidth {
		t.Errorf("after the open the row is %d octets, want %d; the discard is a truncation and not a skip", got, streamRecordWidth)
	}
	if highWater, err := store.StreamHighWater(parts[0], parts[1]); err != nil || highWater != 1 {
		t.Errorf("the repaired row answered (%d, %v), want (1, nil)", highWater, err)
	}

	// the ONE append an allocation performs.
	streamTestAppendOneRecord(t, path, rowName, 2)
	if err := store.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}

	reopened, err := OpenStreamStore(dir)
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	defer reopened.Close()
	if got := reopened.rowWriteCount(); got != 0 {
		t.Errorf("the reopen performed %d row write(s) on a row nothing had torn, want 0", got)
	}
	highWater, err := reopened.StreamHighWater(parts[0], parts[1])
	if err != nil {
		t.Fatalf(
			"after a repair, one append and a restart the row answered %v; a store that skipped the discard leaves R1, half-R2, R2' on disk, which is one failing whole record with a partial after it",
			err,
		)
	}
	if highWater != 2 {
		t.Errorf(
			"after a repair, one append and a restart the row answered %d, want 2; answering 1 here is the index handed out before the restart handed out again",
			highWater,
		)
	}
}

// The closed store answers no index, and specifically not a zero. Contract clause 4's error-free
// zero is correct for a stream never seen and catastrophic for a store that cannot look.
func TestAClosedStoreRefusesRatherThanAnsweringZero(t *testing.T) {
	parts := streamTestKeyOctets(t, 31)
	dir := t.TempDir()
	store, err := OpenStreamStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	highWater, err := store.StreamHighWater(parts[0], parts[1])
	if !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("a closed store answered (%d, %v), want ErrStreamStoreState", highWater, err)
	}
	if highWater != 0 {
		t.Errorf("the refusal carried a high water of %d", highWater)
	}
}

// The store's own directory holds the row directory, and the row directory is a DIFFERENT
// directory from the one the next task's exclusion is held in -- so nothing has to be exempted
// by name from the enumeration, and the rule "an entry in the row directory that is not a row is
// a finding" stays categorical. Task 2a Property 1 names this back.
func TestTheEnumeratedDirectoryIsNotTheStoresOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	if store.rowDir == store.dir {
		t.Fatal("the enumerated row directory IS the store's directory, so the next task's exclusion entry would have to be exempted from the enumeration by name -- an ignore-list, which goes on silently ignoring the second non-row somebody writes there tomorrow")
	}
	if filepath.Dir(store.rowDir) != filepath.Clean(store.dir) {
		t.Errorf("the row directory %s is not directly inside the store's directory %s; a guard outside dir puts this store's lock in a directory the store does not own", store.rowDir, store.dir)
	}

	// an entry beside the row directory is not read by the enumeration at all.
	beside := filepath.Join(dir, "exclusion")
	if err := os.WriteFile(beside, []byte("held by the operating system"), 0o600); err != nil {
		t.Fatalf("write an entry beside the row directory: %v", err)
	}
	// closed first, because Task 2a's exclusion now refuses a second opener of a live
	// directory -- which is the point of it, and which this reopen is not testing.
	if err := store.Close(); err != nil {
		t.Fatalf("close the first store: %v", err)
	}
	reopened := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 41)
	if highWater, err := reopened.StreamHighWater(parts[0], parts[1]); err != nil || highWater != 0 {
		t.Errorf("an entry beside the row directory was answered (%d, %v); the enumeration reads the row directory and nothing else", highWater, err)
	}
}

// A row's records carry a checksum bound to THE ROW'S OWN NAME, so a row moved or copied under
// another key's name does not verify there. Without that binding a row file placed under another
// key's name is adopted whole, and the key it lands on takes that row's high water -- which may
// be BEHIND the index its own ladder has already spent. A high water that moves backwards under
// a key that has already sealed at a higher index is a stream_index handed out twice, which is
// the one thing this store exists to make impossible.
func TestARowsRecordsDoNotVerifyUnderAnotherRowsName(t *testing.T) {
	left := streamTestKeyOctets(t, 61)
	right := streamTestKeyOctets(t, 97)
	leftName := streamTestRowName(t, left)
	rightName := streamTestRowName(t, right)
	if leftName == rightName {
		t.Fatal("the two fixture keys derive one row name, so this case cannot judge anything")
	}

	dir := t.TempDir()
	// the LEFT key's row, written under the RIGHT key's name.
	streamTestPlantRow(t, dir, rightName, streamTestRowBody(leftName, 1, 2, 3))
	store := streamTestOpen(t, dir)

	highWater, err := store.StreamHighWater(right[0], right[1])
	if err == nil && highWater == 3 {
		t.Fatalf(
			"a row written for %s and placed under %s's name was adopted whole and answered high water 3; the record checksum must be bound to the row's own name, or a row moved between keys carries its counter with it",
			leftName,
			rightName,
		)
	}
	if !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("a foreign row's records under this key's name answered (%d, %v); want ErrStreamStoreState", highWater, err)
	}
	if highWater != 0 {
		t.Errorf("the refusal carried a high water of %d", highWater)
	}
}

// messagegroup.StreamKey is the type this store keys on, and this asserts the consumed shape
// rather than trusting the plan's spelling of it. R2.
func TestTheConsumedStreamKeyIsTheOneConnectDeclares(t *testing.T) {
	var key messagegroup.StreamKey
	if got := streamRowName(key); len(got) != streamRowNameLen {
		t.Errorf("the zero key derives a %d-character row name, want %d", len(got), streamRowNameLen)
	}
	keyType := reflect.TypeOf(key)
	for i := range keyType.NumField() {
		field := keyType.Field(i)
		if field.Type.Kind() != reflect.Array || field.Type.Elem().Kind() != reflect.Uint8 {
			t.Errorf(
				"%s.%s is %s; the flattening in this package takes octets for every field and refuses a key it cannot fill, so a field of another kind must arrive as a refusal rather than as a zero value",
				keyType.String(),
				field.Name,
				field.Type.String(),
			)
		}
	}
}

// ----------------------------------------------------------------------------------------------
// The high-water defect: a stream high water that moved BACKWARDS inside one row.
// ----------------------------------------------------------------------------------------------

// streamTestPlantRecordAt overwrites the record at a 1-based position with a WELL-FORMED record
// carrying index, and asserts at the byte level that the plant landed and landed once. A mutation
// read before it is asserted is a mutation nobody ran.
func streamTestPlantRecordAt(t *testing.T, path string, rowName string, position int, index uint64) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s to plant a record: %v", path, err)
	}
	at := (position - 1) * streamRecordWidth
	if len(body) < at+streamRecordWidth {
		t.Fatalf("row %s is %d octets, too short to hold a record at position %d", path, len(body), position)
	}
	record := encodeStreamRecord(rowName, index)
	before := append([]byte(nil), body[at:at+streamRecordWidth]...)
	matchesBefore := bytes.Count(body, record[:])
	copy(body[at:at+streamRecordWidth], record[:])
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the planted row %s: %v", path, err)
	}

	landed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read %s after planting: %v", path, err)
	}
	if len(landed) != len(body) {
		t.Fatalf("the plant changed the row's length from %d to %d; this mutation is in place", len(body), len(landed))
	}
	if !bytes.Equal(landed[at:at+streamRecordWidth], record[:]) {
		t.Fatalf("the planted record is not at offset %d of %s, so this mutation did not land", at, path)
	}
	if bytes.Equal(before, record[:]) {
		t.Fatalf("the planted record equals what was already at position %d, so this mutation changed nothing", position)
	}
	matches := bytes.Count(landed, record[:])
	if matches != matchesBefore+1 {
		t.Fatalf("the planted record's octets occur %d times in %s and occurred %d times before the plant, want exactly one more; a mutation whose match count did not move by one is not the mutation this test reads", matches, path, matchesBefore)
	}
	if !verifyStreamRecord(rowName, landed[at:at+streamRecordWidth]) {
		t.Fatalf("the planted record does not verify under %s, so the store would refuse it as a torn tail and this test would pass for the wrong reason", rowName)
	}
	t.Logf("planted a VERIFYING record carrying index %d at position %d (offset %d) of a %d-octet row; byte-level matches %d -> %d",
		index, position, at, len(landed), matchesBefore, matches)
}

// TestAHighWaterThatMovedBackwardsInsideOneRowIsRefused is the review's HIGH finding, executable.
//
// CLASS: every sequence of verifying records one row can hold.
// SCOPE, derived separately: the sequences reachable through the record checksum. The checksum
// binds the index to the row's NAME and to nothing else, so the set of records that verify in a
// given row is exactly {encodeStreamRecord(rowName, n) : n in u64} -- INDEPENDENT OF POSITION.
// The scope is therefore every function from positions to u64, and the store's rule narrows it to
// the strictly increasing ones that start at 1.
//
// THE COMPLEMENT OF THAT NARROWING, printed rather than asserted non-empty: every sequence that
// is not strictly increasing from at least 1. The three shapes below are its representatives --
// a lower index after a higher one, an index equal to the one before it, and a record carrying
// the zero that clause 4 reserves for a stream never seen. Each is a row whose bytes every
// per-record check in this file accepts.
func TestAHighWaterThatMovedBackwardsInsideOneRowIsRefused(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		planted   []uint64
		position  int
		index     uint64
		wasBefore uint64
	}{
		{
			name:      "a lower index at a later offset",
			planted:   []uint64{1, 2, 3},
			position:  3,
			index:     1,
			wasBefore: 3,
		},
		{
			name:      "the same index twice",
			planted:   []uint64{1, 2, 3},
			position:  3,
			index:     2,
			wasBefore: 3,
		},
		{
			name:      "a record carrying the zero that means never seen",
			planted:   []uint64{1},
			position:  1,
			index:     0,
			wasBefore: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			parts := streamTestKeyOctets(t, 0x40)
			rowName := streamTestRowName(t, parts)
			path := streamTestPlantRow(t, dir, rowName, streamTestRowBody(rowName, testCase.planted...))

			// the row as written is answered, so the refusal below is the plant's.
			before := streamTestOpen(t, dir)
			answered, err := before.StreamHighWater(parts[0], parts[1])
			if err != nil {
				t.Fatalf("the unplanted row %v refused: %v", testCase.planted, err)
			}
			if answered != testCase.wasBefore {
				t.Fatalf("the unplanted row %v answered %d, want %d", testCase.planted, answered, testCase.wasBefore)
			}
			before.Close()

			streamTestPlantRecordAt(t, path, rowName, testCase.position, testCase.index)

			// classifyStreamRow is the decision procedure; read it directly first, so a
			// failure here is not confused with one in the store's plumbing.
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("read the planted row: %v", readErr)
			}
			classified, _, classifyErr := classifyStreamRow(rowName, body)
			if !errors.Is(classifyErr, ErrStreamStoreState) {
				t.Errorf("classifyStreamRow answered high water %d and error %v for a row whose index sequence is %v with %d planted at position %d; want ErrStreamStoreState. A verifying record is accepted wherever it sits, so a sequence that is not strictly increasing is a high water that moved backwards inside one row",
					classified, classifyErr, testCase.planted, testCase.index, testCase.position)
			}

			// and the store answers the same, through both stream methods, because a
			// reserve built on a rewound high water is the reuse itself.
			after := streamTestOpen(t, dir)
			highWater, highWaterErr := after.StreamHighWater(parts[0], parts[1])
			if !errors.Is(highWaterErr, ErrStreamStoreState) {
				t.Errorf("StreamHighWater answered (%d, %v) for the planted row, want ErrStreamStoreState", highWater, highWaterErr)
			}
			reserved, reserveErr := after.ReserveStreamIndex(parts[0], parts[1])
			if !errors.Is(reserveErr, ErrStreamStoreState) {
				t.Errorf("ReserveStreamIndex answered (%d, %v) for the planted row, want ErrStreamStoreState; allocating on a row whose high water regressed hands out an index this store already handed out, and section 5.6 calls a reused stream_index under a reused record_key a total break of both AEADs for that record",
					reserved, reserveErr)
			}
		})
	}
}

// TestTheHighWaterRefusalSurvivesTheOpenTimeRepair pins that the planted row is not quietly
// truncated away by OpenStreamStore's repair and then answered as a shorter, legal row. The
// repair's own rule is that a corrupt body is left exactly as it was found, and a non-monotonic
// sequence is a corrupt body.
func TestTheHighWaterRefusalSurvivesTheOpenTimeRepair(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x41)
	rowName := streamTestRowName(t, parts)
	path := streamTestPlantRow(t, dir, rowName, streamTestRowBody(rowName, 1, 2, 3))
	streamTestPlantRecordAt(t, path, rowName, 3, 2)
	lengthBefore := streamTestRowLength(t, path)

	store := streamTestOpen(t, dir)
	if length := streamTestRowLength(t, path); length != lengthBefore {
		t.Errorf("the open-time repair changed the planted row's length from %d to %d; a corrupt body is left exactly as it was found, and a repair that shortened this one would answer a legal high water for a row that regressed",
			lengthBefore, length)
	}
	if writes := store.rowWriteCount(); writes != 0 {
		t.Errorf("opening over a corrupt body performed %d row writes, want 0", writes)
	}
	if _, err := store.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("after the open-time repair the planted row answered %v, want ErrStreamStoreState", err)
	}
}

// ==============================================================================================
// Task 2 -- Reserve, HighWater, the fsync boundary, and the two sentinels
// ==============================================================================================

// streamTestRowDirSnapshot records every entry in the row directory together with the identity
// the PLATFORM gives that entry, so a later comparison can see a directory-entry mutation that
// leaves the entry set unchanged.
//
// os.SameFile(info, info) is not a tautology here and it is not decoration: on Windows the file
// identity behind a FileInfo is loaded LAZILY, by reopening the recorded PATH, so an identity
// read after a rename-over would be the NEW file's. Forcing the load at snapshot time is what
// makes the comparison a comparison of the entries that existed then. Measured on this machine:
// without the forcing call a rename-over compares equal, with it the comparison is false.
func streamTestRowDirSnapshot(t *testing.T, rowDir string) map[string]os.FileInfo {
	t.Helper()
	entries, err := os.ReadDir(rowDir)
	if err != nil {
		t.Fatalf("snapshot %s: %v", rowDir, err)
	}
	snapshot := map[string]os.FileInfo{}
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(rowDir, entry.Name()))
		if err != nil {
			t.Fatalf("stat %s in %s: %v", entry.Name(), rowDir, err)
		}
		if !os.SameFile(info, info) {
			t.Fatalf("the platform cannot identify %s, so a directory-entry mutation cannot be observed here", entry.Name())
		}
		snapshot[entry.Name()] = info
	}
	return snapshot
}

// streamTestDirEntryMutations is the SECOND of Property 1's two numbers, observed from OUTSIDE
// the store. It is not a counter the store reports, because a self-reported number cannot see a
// rename and a rename is exactly what the mutation this number exists to catch performs.
func streamTestDirEntryMutations(
	t *testing.T,
	rowDir string,
	before map[string]os.FileInfo,
) (creates int, removes int, replacements int) {
	t.Helper()
	after := streamTestRowDirSnapshot(t, rowDir)
	for name, afterInfo := range after {
		beforeInfo, existed := before[name]
		if !existed {
			creates += 1
			continue
		}
		if !os.SameFile(beforeInfo, afterInfo) {
			replacements += 1
		}
	}
	for name := range before {
		if _, survived := after[name]; !survived {
			removes += 1
		}
	}
	return creates, removes, replacements
}

func streamTestSetInterrupt(t *testing.T, store *StreamStore, interrupt streamAppendInterrupt) {
	t.Helper()
	store.stateMutex.Lock()
	defer store.stateMutex.Unlock()
	store.interrupt = interrupt
}

// ----------------------------------------------------------------------------------------------
// Property 1 -- Reserve returns only after the reservation is on stable storage by a mechanism
// this platform can force.
// ----------------------------------------------------------------------------------------------

// CLASS: every forced flush on the allocation path, and every directory-entry mutation on it.
// SCOPE, derived separately: the whole allocation path and everything it calls -- the gate wraps
// the exported ReserveStreamIndex and reads the store's flush counter, which is incremented at
// the flush SITE (TestEveryForcedFlushInTheStoreIsCounted holds that by reading this package's
// syntax tree), so a flush moved into a helper still shows up here.
//
// THE GATE REPORTS TWO NUMBERS OVER TWO CASES, because a correct implementation cannot make the
// second number zero in both. The row for a never-before-seen key does not exist until the store
// creates it and the store has no key set at open time to pre-create from (S2-16), so the FIRST
// allocation against a key necessarily creates a directory entry.
//
//	first allocation for a key   -> one create,  one forced flush
//	every allocation after it    -> zero creates, one forced flush
//
// The second number is what makes the property platform-independent: on Windows the correct
// implementation and a temp-file-and-rename implementation both force exactly ONE flush -- the
// directory flush the rename would need answers "Access is denied" there -- so a gate that
// counted only flushes would be unable to tell them apart on the platform this is written on.
func TestTheAllocationPathsForcedFlushesAndDirectoryEntryMutations(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 0x51)

	for _, reading := range []struct {
		name        string
		wantIndex   uint64
		wantCreates int
	}{
		{name: "first allocation for a key", wantIndex: 1, wantCreates: 1},
		{name: "every allocation after it", wantIndex: 2, wantCreates: 0},
		{name: "and the one after that", wantIndex: 3, wantCreates: 0},
	} {
		t.Run(reading.name, func(t *testing.T) {
			before := streamTestRowDirSnapshot(t, store.rowDir)
			flushesBefore := store.rowFlushCount()

			index, err := store.ReserveStreamIndex(parts[0], parts[1])
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if index != reading.wantIndex {
				t.Fatalf("reserve answered %d, want %d", index, reading.wantIndex)
			}

			flushes := store.rowFlushCount() - flushesBefore
			creates, removes, replacements := streamTestDirEntryMutations(t, store.rowDir, before)
			t.Logf("FORCED FLUSHES on the allocation path: %d; DIRECTORY-ENTRY MUTATIONS: %d (creates %d, removes %d, replacements %d)",
				flushes, creates+removes+replacements, creates, removes, replacements)

			if flushes != 1 {
				t.Errorf("the allocation path forced %d flushes, want exactly 1; clause 1 is that Reserve returns only after the reservation is durable, and a platform-independent statement of it has exactly one member here because an allocation against an existing row mutates no directory entry",
					flushes)
			}
			if creates != reading.wantCreates {
				t.Errorf("the allocation path created %d directory entries, want %d", creates, reading.wantCreates)
			}
			if removes != 0 || replacements != 0 {
				t.Errorf("the allocation path removed %d and replaced %d directory entries, want 0 and 0; a rename over the row is a directory-entry mutation whose durability Windows will not force, so a row written that way is a reservation this platform cannot prove it recorded",
					removes, replacements)
			}
		})
	}
}

// A flush error is RETURNED, never swallowed, and the index it failed to record is not handed
// out. A Reserve that returned (n, nil) after a failed flush has handed out an index it cannot
// prove it recorded.
func TestAFailedFlushIsReturnedAndTheIndexIsNotHandedOut(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 0x52)

	streamTestSetInterrupt(t, store, streamAppendFailTheFlush)
	index, err := store.ReserveStreamIndex(parts[0], parts[1])
	if err == nil {
		t.Fatalf("a failed flush answered (%d, nil)", index)
	}
	if !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("a failed flush answered %v, want ErrStreamStoreState", err)
	}
	if index != 0 {
		t.Errorf("a failed flush answered index %d alongside its error; an index returned beside an error is an index a caller may use", index)
	}

	// and the number that failed is never answered by a later call, on this store or the next.
	streamTestSetInterrupt(t, store, streamAppendUninterrupted)
	next, err := store.ReserveStreamIndex(parts[0], parts[1])
	if err != nil {
		t.Fatalf("reserve after a failed flush: %v", err)
	}
	if next != 2 {
		t.Errorf("the allocation after a failed flush answered %d, want 2; the index the failed flush wrote is BURNED -- the server enforces monotonicity and not contiguity, so a gap is legal and a reuse is not", next)
	}
}

// ----------------------------------------------------------------------------------------------
// Property 2 -- StreamHighWater is answered from persisted state and never rewinds.
// ----------------------------------------------------------------------------------------------

func TestStreamHighWaterIsAnsweredFromPersistedStateAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x53)

	store := streamTestOpen(t, dir)
	for want := uint64(1); want <= 3; want += 1 {
		index, err := store.ReserveStreamIndex(parts[0], parts[1])
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if index != want {
			t.Fatalf("reserve answered %d, want %d", index, want)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// the restart: a new store, over the same directory, with no memory of the old one.
	reopened := streamTestOpen(t, dir)
	highWater, err := reopened.StreamHighWater(parts[0], parts[1])
	if err != nil {
		t.Fatalf("high water after a restart: %v", err)
	}
	if highWater != 3 {
		t.Errorf("the reopened store answered a high water of %d, want 3; NewSenderRatchet reads this in its CONSTRUCTOR and walks highWater+1 rungs, so a store that answered a recomputed number would place a live ladder under a counter nothing has recorded", highWater)
	}
	next, err := reopened.ReserveStreamIndex(parts[0], parts[1])
	if err != nil {
		t.Fatalf("reserve after a restart: %v", err)
	}
	if next != 4 {
		t.Errorf("the first allocation after a restart answered %d, want 4; resuming at HighWater() rather than HighWater()+1 hands out an index already spent, and this is invisible without the restart", next)
	}
}

// A crash BETWEEN the flush and Reserve's return burns the index and never reuses it. A crash
// BEFORE the flush burns nothing. Both answers come from the reopened store, and neither of them
// may be a refusal: the unflushed record is a torn tail and Task 1 Property 4 case 2 requires it
// discarded, not refused.
func TestACrashAroundTheFlushBurnsExactlyWhatTheFlushRecorded(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		interrupt     streamAppendInterrupt
		wantHighWater uint64
		wantNext      uint64
	}{
		{
			name:          "a crash after the flush burns the index",
			interrupt:     streamAppendDieAfterFlush,
			wantHighWater: 3,
			wantNext:      4,
		},
		{
			name:          "a crash before the flush burns nothing",
			interrupt:     streamAppendTearBeforeFlush,
			wantHighWater: 2,
			wantNext:      3,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			parts := streamTestKeyOctets(t, 0x54)
			store := streamTestOpen(t, dir)
			for want := uint64(1); want <= 2; want += 1 {
				if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != want {
					t.Fatalf("reserve answered (%d, %v), want (%d, nil)", index, err, want)
				}
			}

			streamTestSetInterrupt(t, store, testCase.interrupt)
			index, err := store.ReserveStreamIndex(parts[0], parts[1])
			if err == nil {
				t.Fatalf("the interrupted allocation answered (%d, nil); a Reserve that did not return is a Reserve that handed out nothing", index)
			}
			if index != 0 {
				t.Errorf("the interrupted allocation answered index %d beside its error", index)
			}
			// the crash: the store's process is gone, so nothing it remembered survives
			// and nothing it held is closed.
			store.Close()

			reopened := streamTestOpen(t, dir)
			highWater, err := reopened.StreamHighWater(parts[0], parts[1])
			if err != nil {
				t.Fatalf("the reopened store refused the crashed row: %v; the unflushed record is a torn tail and a torn tail is discarded, not refused", err)
			}
			if highWater != testCase.wantHighWater {
				t.Errorf("the reopened store answered a high water of %d, want %d", highWater, testCase.wantHighWater)
			}
			next, err := reopened.ReserveStreamIndex(parts[0], parts[1])
			if err != nil {
				t.Fatalf("reserve after the crash: %v", err)
			}
			if next != testCase.wantNext {
				t.Errorf("the first allocation after the crash answered %d, want %d; a burned index is a legal gap, a reused one is section 5.6's total break", next, testCase.wantNext)
			}
		})
	}
}

// Persisted state BEHIND an index this store has already handed out is ErrStreamStoreRewound, on
// both stream methods -- and on the allocator it is ALSO ErrStreamStoreConsumed, because the next
// position is one this store has already returned to a caller and it has no way past it. Both
// names come off one value.
//
// WHAT DETECTS IT is the verified prefix and not a record of what was returned. This case returns
// three indices before the truncation, so it reads the same either way; the two cases that tell
// the detectors apart are TestARowEmptiedInPlaceIsRefusedRatherThanRestartedAtOne, where the
// store has returned nothing at all, and TestTheRefusalForAVanishedRowIsStickyAndRecreatesNothing.
func TestPersistedStateBehindAnIndexAlreadyHandedOutIsRefused(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x55)
	rowName := streamTestRowName(t, parts)
	store := streamTestOpen(t, dir)
	for want := uint64(1); want <= 3; want += 1 {
		if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != want {
			t.Fatalf("reserve answered (%d, %v), want (%d, nil)", index, err, want)
		}
	}

	// the row goes backwards under a live store: two of its three records are removed out of
	// band. Asserted at the byte level before it is read.
	path := filepath.Join(store.rowDir, rowName)
	if length := streamTestRowLength(t, path); length != 3*streamRecordWidth {
		t.Fatalf("the row is %d octets, want %d", length, 3*streamRecordWidth)
	}
	if err := os.Truncate(path, streamRecordWidth); err != nil {
		t.Fatalf("truncate the row: %v", err)
	}
	if length := streamTestRowLength(t, path); length != streamRecordWidth {
		t.Fatalf("the truncation did not land: the row is %d octets, want %d", length, streamRecordWidth)
	}
	t.Logf("the row went from %d to %d octets under a live store: 3 records to 1", 3*streamRecordWidth, streamRecordWidth)

	highWater, err := store.StreamHighWater(parts[0], parts[1])
	if !errors.Is(err, ErrStreamStoreRewound) {
		t.Errorf("StreamHighWater answered (%d, %v) for a row that went backwards, want ErrStreamStoreRewound", highWater, err)
	}
	index, err := store.ReserveStreamIndex(parts[0], parts[1])
	if !errors.Is(err, ErrStreamStoreRewound) {
		t.Errorf("ReserveStreamIndex answered (%d, %v), want ErrStreamStoreRewound", index, err)
	}
	if !errors.Is(err, ErrStreamStoreConsumed) {
		t.Errorf("ReserveStreamIndex answered %v, which errors.Is does not find ErrStreamStoreConsumed in; streamindex.go clause 3 says the permanent refusal is \"what a row that went backwards under a live process looks like from in here\", and SenderRatchet.Next branches on that name", err)
	}
	if index != 0 {
		t.Errorf("a refused allocation answered index %d beside its error", index)
	}
}

// ----------------------------------------------------------------------------------------------
// Property 3 -- no index is ever handed out twice, and the store's PERMANENT refusal is typed.
// ----------------------------------------------------------------------------------------------

func TestAStreamThatHasSpentTheLastIndexAU64HoldsIsPermanentlyRefused(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x56)
	rowName := streamTestRowName(t, parts)
	streamTestPlantRow(t, dir, rowName, streamTestRowBody(rowName, math.MaxUint64))

	store := streamTestOpen(t, dir)
	if highWater, err := store.StreamHighWater(parts[0], parts[1]); err != nil || highWater != math.MaxUint64 {
		t.Fatalf("high water answered (%d, %v), want (%d, nil)", highWater, err, uint64(math.MaxUint64))
	}
	for attempt := 1; attempt <= 3; attempt += 1 {
		index, err := store.ReserveStreamIndex(parts[0], parts[1])
		if !errors.Is(err, ErrStreamStoreConsumed) {
			t.Fatalf("attempt %d answered (%d, %v), want ErrStreamStoreConsumed; a typed fatal error per section 5.9 G7, never a bool and never a log line", attempt, index, err)
		}
		if index != 0 {
			t.Errorf("attempt %d answered index %d beside its error", attempt, index)
		}
	}
	t.Log("the refusal is PERMANENT: three attempts, three refusals, and no later call can make a next position exist")
}

// A transient filesystem failure is NOT the consumed sentinel. Calling it one would tell a
// SenderRatchet to stop forever over a full disk. This is the control for the mutation that
// returns a bare filesystem error where the store is permanently unable to allocate -- and for
// the opposite one, which types every failure as permanent.
func TestATransientFailureIsNotThePermanentRefusal(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x57)
	store := streamTestOpen(t, dir)

	streamTestSetInterrupt(t, store, streamAppendFailTheFlush)
	_, err := store.ReserveStreamIndex(parts[0], parts[1])
	if err == nil {
		t.Fatal("a failed flush was not refused")
	}
	if errors.Is(err, ErrStreamStoreConsumed) {
		t.Errorf("a failed flush answered %v, which errors.Is finds ErrStreamStoreConsumed in; the consumed sentinel is the store's PERMANENT refusal and a flush that failed once is not one", err)
	}
	if errors.Is(err, ErrStreamStoreRewound) {
		t.Errorf("a failed flush answered %v, which errors.Is finds ErrStreamStoreRewound in", err)
	}
}

// ----------------------------------------------------------------------------------------------
// Property 4 -- the store is total over its key space; Property 5 -- Reserve is not idempotent.
// ----------------------------------------------------------------------------------------------

func TestAStreamNeverSeenIsZeroWithNoErrorAndTheFirstAllocationIsOne(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 0x58)

	highWater, err := store.StreamHighWater(parts[0], parts[1])
	if err != nil {
		t.Fatalf("a stream never seen answered %v, want no error; the absence of an error here is what clause 4 requires", err)
	}
	if highWater != 0 {
		t.Fatalf("a stream never seen answered %d, want 0", highWater)
	}
	if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != 1 {
		t.Errorf("the first allocation answered (%d, %v), want (1, nil); section 5.1 makes record_id = 0 the \"from the beginning\" cursor and the two must not disagree in shape", index, err)
	}
}

func TestReserveIsNotIdempotentAndTwoCallsAreTwoIndices(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 0x59)

	seen := map[uint64]int{}
	for call := 1; call <= 8; call += 1 {
		index, err := store.ReserveStreamIndex(parts[0], parts[1])
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		if previous, repeated := seen[index]; repeated {
			t.Fatalf("call %d answered index %d, which call %d already answered; there is no call that answers an index a previous call answered", call, index, previous)
		}
		seen[index] = call
		if uint64(call) != index {
			t.Errorf("call %d answered %d; under allocation two calls are two indices and the ladder is contiguous while nothing fails", call, index)
		}
	}

	// a second key is a second ladder: every field of the key separates a row.
	other := streamTestKeyOctets(t, 0x77)
	if index, err := store.ReserveStreamIndex(other[0], other[1]); err != nil || index != 1 {
		t.Errorf("the first allocation of a second key answered (%d, %v), want (1, nil)", index, err)
	}
}

// ==============================================================================================
// Task 2a -- the single writer, and what a second opener must do
// ==============================================================================================

const streamExclusionHelperDirEnv = "SDK_STREAM_EXCLUSION_HELPER_DIR"
const streamExclusionHelperHoldEnv = "SDK_STREAM_EXCLUSION_HELPER_HOLD"

// TestStreamExclusionHelperProcess is the SECOND PROCESS. It is a helper rather than a test: the
// parent re-executes this test binary with the environment below, because the class Property 1 is
// stated over is "every path by which a second allocator over one directory can come to exist",
// and a second process is a member no in-process gate can reach. A mutant holding the exclusion
// with a package-level sync.Mutex passes every in-process gate there is.
func TestStreamExclusionHelperProcess(t *testing.T) {
	dir := os.Getenv(streamExclusionHelperDirEnv)
	if dir == "" {
		t.Skip("not the helper process: this test runs only when the parent re-executes this binary with " + streamExclusionHelperDirEnv)
	}
	store, err := OpenStreamStore(dir)
	if err != nil {
		fmt.Printf("HELPER-REFUSED %v\n", err)
		os.Exit(0)
	}
	fmt.Println("HELPER-OPENED")
	if os.Getenv(streamExclusionHelperHoldEnv) != "" {
		// hold the exclusion until the parent closes this process's stdin, then die
		// WITHOUT calling Close. Property 2 is that the death releases it and that
		// nothing else does; os.Exit here is what makes the death the only release.
		one := make([]byte, 1)
		os.Stdin.Read(one)
	}
	_ = store
	os.Exit(0)
}

// streamTestHelperProcess starts the helper and returns its stdin (to end it) and the first
// marker line it printed.
func streamTestHelperProcess(t *testing.T, dir string, hold bool) (io.WriteCloser, string, *exec.Cmd) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=TestStreamExclusionHelperProcess", "-test.timeout=60s")
	command.Env = append(os.Environ(), streamExclusionHelperDirEnv+"="+dir)
	if hold {
		command.Env = append(command.Env, streamExclusionHelperHoldEnv+"=1")
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin: %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start the helper: %v", err)
	}
	t.Cleanup(func() {
		stdin.Close()
		command.Wait()
	})

	lines := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(stdout)
		for {
			line, err := reader.ReadString('\n')
			if strings.HasPrefix(line, "HELPER-") {
				lines <- strings.TrimSpace(line)
				return
			}
			if err != nil {
				lines <- "HELPER-NO-MARKER"
				return
			}
		}
	}()
	select {
	case marker := <-lines:
		return stdin, marker, command
	case <-time.After(60 * time.Second):
		t.Fatal("the helper process printed no marker within 60s")
		return nil, "", nil
	}
}

// ----------------------------------------------------------------------------------------------
// Property 1 -- at most one StreamStore allocates against one directory at a time, and a second
// opener is REFUSED rather than admitted.
// ----------------------------------------------------------------------------------------------

// CLASS: every path by which a second allocator over one directory can come to exist.
// SCOPE, derived separately: THE DIRECTORY, not the process. The class has two members here -- a
// second OpenStreamStore inside this process, and a second process opening the same directory --
// and this gate exercises both and reports the count. A gate that held only the first is
// measuring a mutex, and a mutex is invisible to the second process, which is the case CP3b's
// two clients actually create.
//
// AND THE FIFTH THING THIS GATE REPORTS, because it is what keeps this task from breaking Task 1:
// WHERE THE GUARD ENTRY SITS. It reports the path the exclusion was acquired on together with the
// path the enumeration reads, so a guard that moved into the enumerated directory is visible as
// two numbers a reader can compare rather than as a StreamHighWater refusing three tasks later.
func TestAtMostOneStoreAllocatesAgainstOneDirectory(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 0x61)
	if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != 1 {
		t.Fatalf("the holder's own allocation answered (%d, %v)", index, err)
	}

	guardPath := streamStoreGuardPath(dir)
	t.Logf("EXCLUSION ACQUIRED ON: %s", guardPath)
	t.Logf("ENUMERATION READS:     %s", store.rowDir)
	if inside, err := filepath.Rel(store.rowDir, guardPath); err == nil && !strings.HasPrefix(inside, "..") {
		t.Errorf("the guard entry %s is INSIDE the enumerated row directory %s; this store's rule that every entry there is a row is categorical, so the guard would be read as data and every StreamHighWater after a successful open would refuse",
			guardPath, store.rowDir)
	}
	if filepath.Dir(guardPath) != filepath.Clean(dir) {
		t.Errorf("the guard entry %s does not sit directly in the store's own directory %s", guardPath, dir)
	}

	paths := 0

	// member 1 of the class: a second OpenStreamStore inside this process.
	paths += 1
	second, err := OpenStreamStore(dir)
	if !errors.Is(err, ErrStreamStoreLocked) {
		if err == nil {
			// admitted. Say what that COSTS rather than only that it happened:
			// both stores read the same persisted high water and allocate the
			// same next index.
			mine, mineErr := store.ReserveStreamIndex(parts[0], parts[1])
			theirs, theirsErr := second.ReserveStreamIndex(parts[0], parts[1])
			second.Close()
			t.Errorf("a second OpenStreamStore in this process was ADMITTED; it then allocated %d (%v) while the first allocated %d (%v) -- equal: %v. A reused stream_index is a reused nonce under a reused record_key, which spec A section 5.6 calls a total break of both AEADs for that record",
				theirs, theirsErr, mine, mineErr, mine == theirs)
		}
		t.Errorf("a second OpenStreamStore in this process answered %v, want ErrStreamStoreLocked; two stores over one directory each read the same persisted high water and each allocate the same next index", err)
	} else if !strings.Contains(err.Error(), dir) {
		t.Errorf("the refusal %v does not name the directory it refused", err)
	}

	// member 2 of the class: a second PROCESS.
	paths += 1
	_, marker, _ := streamTestHelperProcess(t, dir, false)
	if !strings.HasPrefix(marker, "HELPER-REFUSED") {
		t.Errorf("a second process answered %q, want a refusal; a package-level mutex is invisible to it", marker)
	} else if !strings.Contains(marker, ErrStreamStoreLocked.Error()) {
		t.Errorf("the second process's refusal %q is not ErrStreamStoreLocked", marker)
	}

	t.Logf("PATHS EXERCISED: %d of the 2 the class has -- a second OpenStreamStore in this process, and a second process", paths)
	if paths != 2 {
		t.Errorf("the gate exercised %d paths, want 2", paths)
	}
}

// The exclusion is held for the LIFE OF THE STORE and released at Close, never at the end of an
// allocation. A store that released it per call leaves every gap between two allocations open to
// a second allocator.
func TestTheExclusionIsHeldAcrossAllocationsAndReleasedAtClose(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 0x62)
	for want := uint64(1); want <= 3; want += 1 {
		if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != want {
			t.Fatalf("reserve answered (%d, %v), want (%d, nil)", index, err, want)
		}
		if second, err := OpenStreamStore(dir); !errors.Is(err, ErrStreamStoreLocked) {
			if err == nil {
				second.Close()
			}
			t.Fatalf("after allocation %d a second opener answered %v, want ErrStreamStoreLocked", want, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := OpenStreamStore(dir)
	if err != nil {
		t.Fatalf("after Close the directory is still refused: %v", err)
	}
	reopened.Close()
}

// ----------------------------------------------------------------------------------------------
// Property 2 -- the exclusion is released by the death of the process that held it, and by
// nothing else.
// ----------------------------------------------------------------------------------------------

func TestTheExclusionIsReleasedByProcessDeathAndByNothingElse(t *testing.T) {
	dir := t.TempDir()

	// a live holder in another process leaves a directory no other process can open.
	stdin, marker, command := streamTestHelperProcess(t, dir, true)
	if marker != "HELPER-OPENED" {
		t.Fatalf("the holder process answered %q, want HELPER-OPENED", marker)
	}
	if second, err := OpenStreamStore(dir); !errors.Is(err, ErrStreamStoreLocked) {
		if err == nil {
			second.Close()
		}
		t.Fatalf("a directory a live process holds answered %v, want ErrStreamStoreLocked", err)
	}

	// the holder dies WITHOUT calling Close. The release is the death's.
	stdin.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("the holder process: %v", err)
	}
	store, err := OpenStreamStore(dir)
	if err != nil {
		t.Fatalf("after the holder died the directory is still refused: %v; a lock that survives its holder's death wedges every later open of that directory, which is exactly what a pid-and-timestamp lock file does", err)
	}
	defer store.Close()
	parts := streamTestKeyOctets(t, 0x63)
	if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != 1 {
		t.Errorf("the store that took over answered (%d, %v), want (1, nil)", index, err)
	}
}

// The finding Property 2 owes is a STALE-LOCK HEURISTIC: any code that decides whether the holder
// is alive by reading a pid, a timestamp or a file's age. This reads the exclusion's own source
// for the shapes such a heuristic is written in, and PRINTS what it looked for, so a reader can
// see the complement rather than take "it passed" on trust.
func TestTheExclusionCarriesNoStaleLockHeuristic(t *testing.T) {
	// Every shape here must be MATCHABLE against what streamTestStripComments produces -- a
	// space-separated stream of identifiers, selectors and literals with no punctuation in it
	// -- and the assertion below is why that is stated rather than assumed. A first version of
	// this list carried "Stat(", which cannot occur in a stream with no parentheses in it: a
	// clause driven by nothing, passing forever, on a gate whose whole job is to notice
	// something.
	forbidden := []string{
		"os.Getpid", "syscall.Getpid", "Getppid",
		"time.Now", "time.Since", "ModTime", "time.Duration",
		"os.ReadFile", "io.ReadAll", "os.Stat",
	}
	for _, shape := range forbidden {
		if strings.ContainsAny(shape, "()[]{}\"") {
			t.Errorf("the forbidden shape %q carries punctuation that streamTestStripComments never emits, so this clause can never fire", shape)
		}
	}
	sources := []string{
		"message_stream_exclusion_windows.go",
		"message_stream_exclusion_unix.go",
		"message_stream_exclusion_other.go",
	}
	read := 0
	for _, source := range sources {
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		read += 1
		body := streamTestStripComments(t, source, content)
		for _, shape := range forbidden {
			if strings.Contains(body, shape) {
				t.Errorf("%s names %q outside a comment; the exclusion has no liveness oracle -- it either survives a crash and wedges every later open of that directory, or it is stolen from a live writer on a heuristic, and the SDK cannot tell those apart",
					source, shape)
			}
		}
	}
	if read != 3 {
		t.Fatalf("this gate read %d exclusion sources, want 3; one declaration per GOOS class and a fallback", read)
	}
	t.Logf("read %d exclusion sources and looked for %d heuristic shapes: %v", read, len(forbidden), forbidden)
}

// streamTestStripComments returns a file's source with every comment removed, so a gate that
// looks for a shape in code is not answered by a comment ABOUT that shape.
func streamTestStripComments(t *testing.T, name string, content []byte) string {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, name, content, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var body strings.Builder
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Comment:
			return false
		case *ast.Ident:
			body.WriteString(typed.Name)
			body.WriteString(" ")
		case *ast.SelectorExpr:
			if pkg, ok := typed.X.(*ast.Ident); ok {
				body.WriteString(pkg.Name + "." + typed.Sel.Name)
				body.WriteString(" ")
			}
		case *ast.BasicLit:
			body.WriteString(typed.Value)
			body.WriteString(" ")
		}
		return true
	})
	return body.String()
}

// ----------------------------------------------------------------------------------------------
// Property 3 -- Reserve is atomic against every other call on this store, across the read, the
// increment AND the flush.
// ----------------------------------------------------------------------------------------------

// -race CANNOT RUN IN THIS SANDBOX -- CGO_ENABLED=0 and no C compiler -- so this gate holds the
// OUTCOME (two goroutines never get one index, and no query observes an increment before the
// flush that made it durable returned) and does NOT hold the torn-guard property a race build
// would add. That half is not covered here and saying so is the point.
func TestConcurrentReservesOnOneStoreNeverHandOutOneIndexTwice(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 0x64)
	rowName := streamTestRowName(t, parts)

	const writers = 8
	const each = 24
	indices := make(chan uint64, writers*each)
	queries := make(chan uint64, writers*each)
	var started sync.WaitGroup
	var done sync.WaitGroup
	started.Add(writers)
	done.Add(writers)
	for writer := 0; writer < writers; writer += 1 {
		go func() {
			defer done.Done()
			started.Done()
			started.Wait()
			for call := 0; call < each; call += 1 {
				index, err := store.ReserveStreamIndex(parts[0], parts[1])
				if err != nil {
					t.Errorf("concurrent reserve: %v", err)
					return
				}
				indices <- index
				highWater, err := store.StreamHighWater(parts[0], parts[1])
				if err != nil {
					t.Errorf("concurrent high water: %v", err)
					return
				}
				queries <- highWater
			}
		}()
	}
	done.Wait()
	close(indices)
	close(queries)

	seen := map[uint64]bool{}
	highest := uint64(0)
	for index := range indices {
		if seen[index] {
			t.Fatalf("index %d was handed out twice; a reused stream_index is a reused nonce under a reused record_key", index)
		}
		seen[index] = true
		if highest < index {
			highest = index
		}
	}
	if len(seen) != writers*each {
		t.Fatalf("%d goroutines times %d calls produced %d distinct indices", writers, each, len(seen))
	}
	if highest != writers*each {
		t.Errorf("the highest index handed out is %d, want %d; a gap means a read and an increment were not one statement", highest, writers*each)
	}

	// every query observed a high water that was already on the disk, which is what taking
	// the row lock across the flush buys.
	for observed := range queries {
		if writers*each < int(observed) {
			t.Fatalf("a query observed a high water of %d, above the %d indices ever allocated", observed, writers*each)
		}
	}

	// and the row itself holds exactly one strictly increasing record per index.
	body, err := os.ReadFile(filepath.Join(store.rowDir, rowName))
	if err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if len(body) != writers*each*streamRecordWidth {
		t.Fatalf("the row is %d octets, want %d; a row shorter than the indices handed out is a reservation that did not reach the disk", len(body), writers*each*streamRecordWidth)
	}
	highWater, _, err := classifyStreamRow(rowName, body)
	if err != nil {
		t.Fatalf("the row written concurrently does not classify: %v", err)
	}
	if highWater != uint64(writers*each) {
		t.Errorf("the row's high water is %d, want %d", highWater, writers*each)
	}
}

// ----------------------------------------------------------------------------------------------
// The mutation that reproduces the 2026-09-09 collision, kept as a test: a guard entry INSIDE the
// enumerated row directory is a finding under Task 1 Property 2's categorical rule, and the
// exclusion still holds -- which is the point. The two properties are only mutually satisfiable
// because the guard sits beside the row directory rather than in it.
// ----------------------------------------------------------------------------------------------

func TestAGuardEntryInsideTheRowDirectoryWouldBeAFinding(t *testing.T) {
	dir := t.TempDir()
	store := streamTestOpen(t, dir)
	parts := streamTestKeyOctets(t, 0x65)
	if _, err := store.StreamHighWater(parts[0], parts[1]); err != nil {
		t.Fatalf("the store refuses before anything is planted: %v", err)
	}

	inside := filepath.Join(store.rowDir, streamGuardName)
	if err := os.WriteFile(inside, nil, 0o600); err != nil {
		t.Fatalf("plant a guard entry inside the row directory: %v", err)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("the plant did not land: %v", err)
	}
	t.Logf("planted %q inside the enumerated directory %s", streamGuardName, store.rowDir)

	_, err := store.StreamHighWater(parts[0], parts[1])
	if !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("an entry in the row directory that is not a row answered %v, want ErrStreamStoreState; the rule is categorical with no name exempted from it, which is why the guard may not live there", err)
	}
	if !strings.Contains(err.Error(), streamGuardName) {
		t.Errorf("the refusal %v does not name the entry it refused", err)
	}
}

// ----------------------------------------------------------------------------------------------
// gates over this package's own source
// ----------------------------------------------------------------------------------------------

// Every forced flush is counted at its call site. The flush counter is one of Property 1's two
// numbers, and a counter that can drift from the flush it counts is a claim rather than a
// measurement.
func TestEveryForcedFlushInTheStoreIsCounted(t *testing.T) {
	const source = "message_stream_store.go"

	// every Sync call site in the package's production source, keyed by its file and the declaration
	// it sits in, which is what streamStoreSyncRulings names.
	sites := map[string]int{}
	for _, name := range streamTestProductionSources(t) {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, name, content, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			function, ok := node.(*ast.FuncDecl)
			if !ok {
				return true
			}
			ast.Inspect(function.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sync" {
					sites[name+" "+messageFragmentEnclosing(parsed, call.Pos())] += 1
				}
				return true
			})
			return false
		})
	}
	const flush = "message_stream_store.go StreamStore.forceFlush"
	t.Logf("Sync call sites in this package's production source, by file and declaration: %v", sites)
	excused := []string{}
	unexcused := map[string]int{}
	for site, count := range sites {
		if site == flush {
			continue
		}
		if _, ruled := streamStoreSyncRulings[site]; ruled {
			excused = append(excused, site)
			continue
		}
		unexcused[site] = count
	}
	stale := []string{}
	for site := range streamStoreSyncRulings {
		if _, there := sites[site]; !there {
			stale = append(stale, site)
		}
	}
	slices.Sort(excused)
	slices.Sort(stale)
	t.Logf("of those, EXCUSED by a ruling of their own as not the store's: %d %v", len(excused), excused)
	if sites[flush] != 1 || len(unexcused) != 0 {
		t.Errorf("this package has %v Sync call site(s) that are neither %s nor ruled in streamStoreSyncRulings, and %d in %s; "+
			"the store's one Sync must be forceFlush's, which is what makes the flush counter a count of flushes PERFORMED "+
			"rather than of flushes intended. If a site is not the stream store's, rule on it there, by the key printed",
			unexcused, flush, sites[flush], flush)
	}
	if len(stale) != 0 {
		t.Errorf("%d ruling(s) in streamStoreSyncRulings excuse a Sync that is not there: %v. An excuse for something "+
			"that is gone is an excuse nothing checks", len(stale), stale)
	}

	// and forceFlush counts, after the call returns.
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, source, content, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}
	counted := false
	sawSyncFirst := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		function, ok := node.(*ast.FuncDecl)
		if !ok || function.Name.Name != "forceFlush" {
			return true
		}
		for _, statement := range function.Body.List {
			if streamTestStatementCallsSync(statement) {
				sawSyncFirst = true
			}
			if streamTestStatementAssigns(statement, "rowFlushes") {
				counted = sawSyncFirst
			}
		}
		return false
	})
	if !counted {
		t.Error("forceFlush does not increment rowFlushes after its Sync returns")
	}
}

// streamTestProductionSources is every non-test .go file in this package, whatever GOOS it is
// constrained to: the gates below read source rather than compile it, so a file this build
// excludes is still read.
// Sync call sites in package sdk that are NOT the stream store's, excused one at a time and by name,
// the way messageFragmentPartSizeCopyRulings excuses copies of the part size: keyed by the file and
// the declaration the call sits in, each with a sentence, and each asserted to match something.
//
// Every entry arrived with the merge of upstream urnetwork/sdk main (msgrepo ledger 277). Before it
// the store's forceFlush held the only Sync in the package and this gate said so in those words.
// Package sdk is upstream's package, and upstream's own persistence syncs files of its own. A Sync
// added anywhere in the package still fails the gate until somebody rules on it, which is the
// decision being asked for rather than skipped.
var streamStoreSyncRulings = map[string]string{
	"device_rpc.go addListenerWithRpcCall": "deviceRemote.Sync() is DeviceRemote's own method, which wakes its " +
		"reconnect monitor to publish listener state. It is not a file sync at all: it is in the class because " +
		"the class is a selector named Sync",
	"local_state_auth.go LocalState.writeAuthStateLocked": "the auth state's atomic write: a temp file synced " +
		"before its rename, then its directory. Upstream's local state, with no flush counter of the store's in it",
	"local_state_location.go LocalState.writePreferenceBytesWithLock": "a preference file's atomic write, the " +
		"same shape for the same reason. Upstream's local state",
	"peer_client_key_pin_store_bounded.go boundedPeerClientKeyPinStore.persistWithLock": "the peer key-pin " +
		"store's commit: its file and then its directory. Upstream's store, not this one",
}

func streamTestProductionSources(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sources := []string{}
	for _, name := range names {
		if !strings.HasSuffix(name, "_test.go") {
			sources = append(sources, name)
		}
	}
	if len(sources) == 0 {
		t.Fatal("this gate read no production source, so it is holding nothing")
	}
	return sources
}

func streamTestStatementAssigns(statement ast.Stmt, field string) bool {
	found := false
	ast.Inspect(statement, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, target := range assign.Lhs {
			if selector, ok := target.(*ast.SelectorExpr); ok && selector.Sel.Name == field {
				found = true
			}
		}
		return true
	})
	return found
}

func streamTestStatementCallsSync(statement ast.Stmt) bool {
	found := false
	ast.Inspect(statement, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "Sync" {
			found = true
		}
		return true
	})
	return found
}

func streamTestStatementCalls(statement ast.Stmt, name string) bool {
	found := false
	ast.Inspect(statement, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == name {
			found = true
		}
		return true
	})
	return found
}

// The injected failure point is a TEST hook. A hook production could set would be a durability
// property with an off switch, so nothing outside a _test.go file may assign it.
func TestNoProductionSourceSetsTheAppendInterrupt(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	production := 0
	assignments := []string{}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		production += 1
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, source, content, 0)
		if err != nil {
			// a file this build's constraints exclude still parses; a parse failure is a
			// real one.
			t.Fatalf("parse %s: %v", source, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, target := range assign.Lhs {
				selector, ok := target.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "interrupt" {
					assignments = append(assignments, fmt.Sprintf("%s", fileSet.Position(assign.Pos())))
				}
			}
			return true
		})
	}
	if production == 0 {
		t.Fatal("this gate read no production source, so it is holding nothing")
	}
	t.Logf("read %d production sources in this package; assignments to an interrupt field: %d", production, len(assignments))
	if 0 < len(assignments) {
		t.Errorf("production source assigns the append interrupt at %v", assignments)
	}
}

// The exclusion's build constraints are the GOOS set on which the PRIMITIVE is declared, and the
// fallback's is that set's complement. The `unix` build term is the wrong constituency and this
// gate says why in the number it prints: solaris and aix satisfy `unix` and declare no
// syscall.Flock, so a file constrained with `unix` is a BUILD BREAK there rather than a fall
// through to the refusal.
func TestTheExclusionBuildConstraintsAreDerivedFromThePrimitive(t *testing.T) {
	flockGoos := streamTestBuildTerms(t, "message_stream_exclusion_unix.go")
	fallback := streamTestBuildTerms(t, "message_stream_exclusion_other.go")
	windows := streamTestBuildTerms(t, "message_stream_exclusion_windows.go")

	t.Logf("flock GOOS terms (%d): %v", len(flockGoos.positive), flockGoos.positive)
	t.Logf("windows terms (%d): %v", len(windows.positive), windows.positive)
	t.Logf("fallback NEGATED terms (%d): %v", len(fallback.negative), fallback.negative)

	if slices.Contains(flockGoos.positive, "unix") {
		t.Error("the flock file is constrained with the `unix` term; go/build's `unix` covers solaris and aix, and syscall.Flock is declared on neither, so this does not send them to the fail-closed file -- it makes them `undefined: syscall.Flock`")
	}
	for _, wrong := range []string{"solaris", "aix"} {
		if slices.Contains(flockGoos.positive, wrong) {
			t.Errorf("the flock file claims %s, which declares no syscall.Flock", wrong)
		}
	}
	if len(flockGoos.positive) == 0 {
		t.Fatal("the flock file names no GOOS at all")
	}
	if len(windows.positive) != 1 || windows.positive[0] != "windows" {
		t.Errorf("the windows file's terms are %v, want exactly [windows]", windows.positive)
	}

	// the fallback's constraint must be the EXACT complement of the other two, so no GOOS
	// gets two implementations and none gets zero.
	covered := append(append([]string{}, flockGoos.positive...), windows.positive...)
	slices.Sort(covered)
	negated := append([]string{}, fallback.negative...)
	slices.Sort(negated)
	if !slices.Equal(covered, negated) {
		t.Errorf("the fallback negates %v and the two implementations cover %v; a GOOS in neither gets no acquireStreamStoreExclusion at all and a GOOS in both gets two", negated, covered)
	}
	if 0 < len(fallback.positive) {
		t.Errorf("the fallback carries positive terms %v; its constraint is a complement and nothing else", fallback.positive)
	}

	// AND THE FALLBACK'S BODY IS A REFUSAL, not a placeholder. A build tag that quietly
	// compiled to a no-op returning a nil closer and a nil error is the single-writer
	// property deleted by a build constraint, and no gate on any platform that HAS the
	// primitive would ever run there to say so.
	fallbackSource, err := os.ReadFile("message_stream_exclusion_other.go")
	if err != nil {
		t.Fatal(err)
	}
	fallbackBody := streamTestStripComments(t, "message_stream_exclusion_other.go", fallbackSource)
	if !strings.Contains(fallbackBody, "ErrStreamStoreLocked") {
		t.Error("the fail-closed file's body does not name ErrStreamStoreLocked; a platform this store cannot make safe is a platform it refuses to open on")
	}
	// read as SYNTAX rather than as text: every return in the fallback's acquire must carry a
	// non-nil error, so a no-op cannot be smuggled in as a differently spelled nil.
	fallbackSet := token.NewFileSet()
	fallbackParsed, err := parser.ParseFile(fallbackSet, "message_stream_exclusion_other.go", fallbackSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	returns := 0
	ast.Inspect(fallbackParsed, func(node ast.Node) bool {
		function, ok := node.(*ast.FuncDecl)
		if !ok || function.Name.Name != "acquireStreamStoreExclusion" {
			return true
		}
		ast.Inspect(function.Body, func(inner ast.Node) bool {
			statement, ok := inner.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			returns += 1
			if len(statement.Results) != 2 {
				t.Errorf("a return in the fail-closed acquire has %d results, want 2", len(statement.Results))
				return true
			}
			if identifier, ok := statement.Results[1].(*ast.Ident); ok && identifier.Name == "nil" {
				t.Errorf("the fail-closed acquire returns a nil error at %s; a platform this store cannot make safe is a platform it refuses to open on, and a build tag that quietly compiled to a no-op is the single-writer property deleted by a build constraint",
					fallbackSet.Position(statement.Pos()))
			}
			return true
		})
		return false
	})
	if returns == 0 {
		t.Fatal("the fail-closed file declares no acquireStreamStoreExclusion, so this gate read nothing")
	}
	t.Logf("the fail-closed acquire has %d return statement(s), none of them with a nil error", returns)

	// THE COMPLEMENT, PRINTED. These are the platforms that get the refusal.
	refused := []string{}
	for _, goos := range []string{
		"aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "js",
		"linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows",
	} {
		implied := map[string][]string{"android": {"linux"}, "ios": {"darwin"}}[goos]
		terms := append([]string{goos}, implied...)
		held := false
		for _, term := range terms {
			if slices.Contains(covered, term) {
				held = true
			}
		}
		if !held {
			refused = append(refused, goos)
		}
	}
	t.Logf("THE COMPLEMENT -- GOOS values that get the fail-closed refusal (%d): %v", len(refused), refused)
	if len(refused) == 0 {
		t.Error("the fail-closed file has no constituency at all, so it is never compiled and its refusal is never anybody's answer; an empty complement here means the narrowing was read wrong")
	}
	for _, want := range []string{"js", "solaris", "aix", "plan9", "wasip1"} {
		if !slices.Contains(refused, want) {
			t.Errorf("%s is not in the fail-closed complement", want)
		}
	}
}

type streamBuildConstraint struct {
	positive []string
	negative []string
}

func streamTestBuildTerms(t *testing.T, name string) streamBuildConstraint {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	constraint := streamBuildConstraint{}
	found := false
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "//go:build ") {
			continue
		}
		found = true
		for _, term := range strings.Fields(strings.TrimPrefix(line, "//go:build ")) {
			switch {
			case term == "||" || term == "&&":
			case strings.HasPrefix(term, "!"):
				constraint.negative = append(constraint.negative, strings.TrimPrefix(term, "!"))
			default:
				constraint.positive = append(constraint.positive, term)
			}
		}
	}
	if !found {
		t.Fatalf("%s carries no //go:build line, so its constituency is whatever its filename implies", name)
	}
	return constraint
}

// ----------------------------------------------------------------------------------------------
// the join between the two repos, stated mechanically rather than by eye
// ----------------------------------------------------------------------------------------------

// streamStoreMethodShape is a COMPILE-TIME assertion of the store's two stream methods, exactly
// as this task produces them. It fails to build if either name or either signature moves.
var streamStoreMethodShape = struct {
	reserve   func(*StreamStore, []byte, []byte) (uint64, error)
	highWater func(*StreamStore, []byte, []byte) (uint64, error)
}{
	reserve:   (*StreamStore).ReserveStreamIndex,
	highWater: (*StreamStore).StreamHighWater,
}

// streamReserverMethodShape is a COMPILE-TIME assertion of the interface's two methods as
// connect/messagegroup declares them TODAY. It fails to build if either name or either signature
// moves there.
var streamReserverMethodShape = func(reserver messagegroup.StreamIndexReserver) (
	func(messagegroup.StreamKey) (uint64, error),
	func(messagegroup.StreamKey) (uint64, error),
) {
	return reserver.Reserve, reserver.HighWater
}

// TestStreamStoreDoesNotYetSatisfyStreamIndexReserver reports the join between sdk and connect as
// a fact rather than an impression. It is checked, not eyeballed: the two var declarations above
// are compile-time assertions of the two method sets, and the reflection below reports the
// difference between them mechanically.
//
// THE ANSWER IS NO, AND THAT IS THE PLAN'S DESIGN RATHER THAN A GAP. Spec A section 8.2 says the
// flattening from these two []byte parameters to messagegroup's comparable StreamKey "is the
// implementer's", and Task 3 declares its adapter "the only code that maps a store failure onto
// messagegroup's sentinels" -- so a *StreamStore that satisfied the interface directly would put
// a second flattening and a second mapping in this package, which is what Task 3 exists to
// prevent. A compile-time `var _ messagegroup.StreamIndexReserver = (*StreamStore)(nil)` here
// would not compile, on two counts at once, and the two counts are printed below.
func TestStreamStoreDoesNotYetSatisfyStreamIndexReserver(t *testing.T) {
	_ = streamStoreMethodShape
	_ = streamReserverMethodShape

	reserverType := reflect.TypeOf((*messagegroup.StreamIndexReserver)(nil)).Elem()
	storeType := reflect.TypeOf((*StreamStore)(nil))
	if reserverType.NumMethod() == 0 {
		t.Fatal("messagegroup.StreamIndexReserver declares no method, so this gate compares nothing")
	}

	satisfied := storeType.Implements(reserverType)
	missing := []string{}
	for i := range reserverType.NumMethod() {
		method := reserverType.Method(i)
		have, ok := storeType.MethodByName(method.Name)
		if !ok {
			missing = append(missing, fmt.Sprintf("%s%s -- *StreamStore declares no method of that name",
				method.Name, method.Type.String()))
			continue
		}
		missing = append(missing, fmt.Sprintf("%s%s -- *StreamStore has %s%s",
			method.Name, method.Type.String(), have.Name, have.Type.String()))
	}
	t.Logf("*sdk.StreamStore satisfies messagegroup.StreamIndexReserver: %v", satisfied)
	t.Logf("THE COMPLEMENT -- what the interface asks for and what this task produces (%d methods):", reserverType.NumMethod())
	for _, line := range missing {
		t.Logf("  %s", line)
	}
	t.Logf("  and the store's own pair: ReserveStreamIndex(groupId, senderHandle []byte) (uint64, error), StreamHighWater(groupId, senderHandle []byte) (uint64, error)")

	if satisfied {
		t.Error("*StreamStore satisfies messagegroup.StreamIndexReserver directly. Task 3 declares its adapter the ONLY code in sdk that flattens a StreamKey and the ONLY code that maps a store failure onto messagegroup's sentinels; a store that satisfies the interface itself is a second one of each")
	}
	for _, name := range []string{"ReserveStreamIndex", "StreamHighWater"} {
		if _, ok := storeType.MethodByName(name); !ok {
			t.Errorf("*StreamStore has no %s; section 8.2 spells the store's two methods with these names", name)
		}
	}
}

// ----------------------------------------------------------------------------------------------
// a row that went backwards under the only writer, which is the rewind detector's whole subject
// ----------------------------------------------------------------------------------------------

// A ROW EMPTIED IN PLACE IS A REWIND AND NOT A STREAM NEVER SEEN, and the store that has to tell
// them apart has never handed an index out for this row.
//
// This is the case the removed handedOut map could not see. Its horizon was the indices THIS
// store had RETURNED, so a row seeded by the open-time scan -- read, never allocated against --
// had no entry, and emptying it in place answered a high water of 0 with no error. The very next
// allocation then handed out index 1 on a row that had already spent 1, 2 and 3, which is spec A
// section 5.6's total break of both AEADs reached with no corrupt byte anywhere. The verified
// prefix is what sees it, because the prefix is seeded by the scan.
//
// Mutation: delete the length comparison in persistedHighWater and this goes red on the first
// assertion, with a reserve that answers (1, nil).
func TestARowEmptiedInPlaceIsRefusedRatherThanRestartedAtOne(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x67)
	rowName := streamTestRowName(t, parts)

	store := streamTestOpen(t, dir)
	for want := uint64(1); want <= 3; want += 1 {
		if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != want {
			t.Fatalf("reserve answered (%d, %v), want (%d, nil)", index, err, want)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A FRESH STORE. Its verified prefix comes from the open-time scan and it has returned
	// nothing, which is the state the removed detector was blind in.
	reopened := streamTestOpen(t, dir)
	path := filepath.Join(reopened.rowDir, rowName)
	if highWater, err := reopened.StreamHighWater(parts[0], parts[1]); err != nil || highWater != 3 {
		t.Fatalf("the reopened store answered (%d, %v), want (3, nil)", highWater, err)
	}
	if length := streamTestRowLength(t, path); length != 3*streamRecordWidth {
		t.Fatalf("the row is %d octets, want %d", length, 3*streamRecordWidth)
	}

	// out of band: the row is emptied IN PLACE. The directory entry survives, so nothing about
	// the row's presence has changed.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("empty the row: %v", err)
	}
	if length := streamTestRowLength(t, path); length != 0 {
		t.Fatalf("the truncation did not land: the row is %d octets, want 0", length)
	}
	t.Logf("the row went from %d octets to 0 in place, under a store that holds the only writer's exclusion and has handed out nothing", 3*streamRecordWidth)

	index, err := reopened.ReserveStreamIndex(parts[0], parts[1])
	if !errors.Is(err, ErrStreamStoreRewound) {
		t.Errorf("the allocation after the row was emptied answered (%d, %v), want ErrStreamStoreRewound; answering 1 here re-issues record_key[1] under a class key that has not moved, which is a repeated (key, nonce) on BOTH of a record's aeads", index, err)
	}
	if !errors.Is(err, ErrStreamStoreConsumed) {
		t.Errorf("the allocation answered %v, which errors.Is does not find ErrStreamStoreConsumed in; the next position is one this row has already spent and no later call can make another", err)
	}
	if index != 0 {
		t.Errorf("a refused allocation answered index %d beside its error", index)
	}
	if highWater, err := reopened.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreRewound) {
		t.Errorf("the query answered (%d, %v), want ErrStreamStoreRewound", highWater, err)
	} else if errors.Is(err, ErrStreamStoreConsumed) {
		t.Errorf("the query answered %v, which errors.Is finds ErrStreamStoreConsumed in; the reader's seat reports that the number moved and does not claim a ladder is wedged", err)
	}
}

// THE REFUSAL FOR A VANISHED ROW IS STICKY, AND IT CREATES NOTHING.
//
// The refusal used to be taken inside the write, which opened the row with os.O_CREATE BEFORE it
// compared the size -- so the refusal itself recreated the row at zero length, the next call read
// a present empty row, and the call after the refusal handed out index 1 on a row that had spent
// 1, 2 and 3. A refusal that manufactures the state which makes the next call succeed is not a
// refusal. Both halves are asserted: the directory entry, at the byte level, and the second call.
//
// Mutation: make persistedHighWater answer (0, nil) for an absent row it has a prefix for, and
// this goes red on both.
func TestTheRefusalForAVanishedRowIsStickyAndRecreatesNothing(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x68)
	rowName := streamTestRowName(t, parts)

	store := streamTestOpen(t, dir)
	for want := uint64(1); want <= 3; want += 1 {
		if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != want {
			t.Fatalf("reserve answered (%d, %v), want (%d, nil)", index, err, want)
		}
	}
	path := filepath.Join(store.rowDir, rowName)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the row: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removal did not land: %v", err)
	}
	t.Log("the row vanished under a store that holds the only writer's exclusion on it")

	for attempt := 1; attempt <= 3; attempt += 1 {
		index, err := store.ReserveStreamIndex(parts[0], parts[1])
		if !errors.Is(err, ErrStreamStoreRewound) {
			t.Fatalf("attempt %d answered (%d, %v), want ErrStreamStoreRewound on every attempt", attempt, index, err)
		}
		if !errors.Is(err, ErrStreamStoreConsumed) {
			t.Errorf("attempt %d answered %v, which errors.Is does not find ErrStreamStoreConsumed in", attempt, err)
		}
		if index != 0 {
			t.Errorf("attempt %d answered index %d beside its error", attempt, index)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			info, statErr := os.Stat(path)
			size := int64(-1)
			if statErr == nil {
				size = info.Size()
			}
			t.Fatalf("attempt %d RECREATED the row it refused (%d octets); the next read then finds a present, empty row, derives a high water of 0 from it and allocates index 1 on a key that has already spent it", attempt, size)
		}
	}
	t.Log("three attempts, three identical refusals, and no directory entry made by any of them")
}

// AN INTERRUPTED APPEND LEAVES THE ROW ALLOCATABLE BY THE NEXT CALL, and this is the ordering
// property in writeOneRecord, driven.
//
// The verified prefix is what the rewind detector stands on, so it is recorded only after the
// write. Record it before, and an interrupted append leaves the row SHORTER than the prefix the
// store claims to have confirmed: the next read refuses it as a rewind and the row is wedged
// permanently by a failure a retry would have cleared. Measured: moving that assignment above the
// write leaves every other stream case green and turns this one red, with the retry refused as
// ErrStreamStoreRewound and ErrStreamStoreConsumed.
//
// The crash case is NOT this case and does not cover it: TestACrashAroundTheFlushBurnsExactlyWhat
// TheFlushRecorded closes the store and reopens, and a reopened store's prefix comes from the
// disk. What this drives is the SAME store going on afterwards.
func TestAnInterruptedAppendLeavesTheRowAllocatableByTheNextCall(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		interrupt streamAppendInterrupt
		wantNext  uint64
	}{
		{
			name:      "a tear before the flush is repaired in place by the next append",
			interrupt: streamAppendTearBeforeFlush,
			wantNext:  2,
		},
		{
			name:      "a failed flush burns the index it wrote and the next call goes on",
			interrupt: streamAppendFailTheFlush,
			wantNext:  3,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			parts := streamTestKeyOctets(t, 0x69)
			rowName := streamTestRowName(t, parts)
			store := streamTestOpen(t, dir)
			if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != 1 {
				t.Fatalf("the first allocation answered (%d, %v), want (1, nil)", index, err)
			}

			streamTestSetInterrupt(t, store, testCase.interrupt)
			index, err := store.ReserveStreamIndex(parts[0], parts[1])
			if err == nil {
				t.Fatalf("the interrupted allocation answered (%d, nil)", index)
			}
			if index != 0 {
				t.Errorf("the interrupted allocation answered index %d beside its error", index)
			}
			path := filepath.Join(store.rowDir, rowName)
			t.Logf("after the interruption the row is %d octets and the call answered %v",
				streamTestRowLength(t, path), err)

			streamTestSetInterrupt(t, store, streamAppendUninterrupted)
			next, err := store.ReserveStreamIndex(parts[0], parts[1])
			if err != nil {
				t.Fatalf(
					"the retry on the SAME store was refused: %v (rewound=%v consumed=%v). An interruption a retry cannot clear is a row wedged forever by a failure that cost nothing, and the store's own prefix is what wedged it",
					err, errors.Is(err, ErrStreamStoreRewound), errors.Is(err, ErrStreamStoreConsumed),
				)
			}
			if next != testCase.wantNext {
				t.Errorf("the retry answered %d, want %d; a burned index is a legal gap and a reused one is section 5.6's total break", next, testCase.wantNext)
			}
			// and the row the retry left is one a fresh store reads identically.
			if err := store.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			reopened := streamTestOpen(t, dir)
			if highWater, err := reopened.StreamHighWater(parts[0], parts[1]); err != nil || highWater != testCase.wantNext {
				t.Errorf("a fresh store over the same directory answered (%d, %v), want (%d, nil)", highWater, err, testCase.wantNext)
			}
		})
	}
}

// ----------------------------------------------------------------------------------------------
// the priced residual, executable
// ----------------------------------------------------------------------------------------------

// The verified prefix is what makes a high-water read cost the records appended since rather than
// every record ever written. What it costs is stated here rather than absorbed: an out-of-band
// IN-PLACE mutation of a record a live store has already checksummed is not seen by THAT store,
// and IS seen by the next one to open the directory. Both halves are asserted, because a residual
// with only its safe half asserted is a residual nobody has measured.
func TestAnOutOfBandMutationOfAVerifiedPrefixIsMissedUntilTheStoreIsReopened(t *testing.T) {
	dir := t.TempDir()
	parts := streamTestKeyOctets(t, 0x66)
	rowName := streamTestRowName(t, parts)
	store := streamTestOpen(t, dir)
	for want := uint64(1); want <= 3; want += 1 {
		if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != want {
			t.Fatalf("reserve answered (%d, %v)", index, err)
		}
	}
	path := filepath.Join(store.rowDir, rowName)

	// a verifying record carrying a LOWER index, planted over the SECOND record -- inside the
	// prefix this store has already checksummed.
	streamTestPlantRecordAt(t, path, rowName, 2, 1)

	highWater, err := store.StreamHighWater(parts[0], parts[1])
	if err != nil || highWater != 3 {
		t.Errorf("the live store answered (%d, %v) for a row whose verified prefix was mutated under it; the residual this test prices is that it answers 3 with no error, so a change here means the residual moved and the comment that prices it is stale",
			highWater, err)
	}
	t.Logf("MISSED by the live store: it answers %d with error %v, because it does not re-checksum a prefix it has already verified under an exclusion it holds", highWater, err)

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened := streamTestOpen(t, dir)
	if _, err := reopened.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreState) {
		t.Errorf("the REOPENED store answered %v for the same row, want ErrStreamStoreState; the reopen re-verifies from record 1, which is what bounds this residual to one store's lifetime rather than a row's", err)
	}
	t.Log("CAUGHT by the next store to open the directory: the miss lasts one store's lifetime, not a row's")
}

// ----------------------------------------------------------------------------------------------
// the cost of a high-water read, measured rather than asserted
// ----------------------------------------------------------------------------------------------

// BenchmarkStreamReserveAtDepth is the review's linear-rescan finding, measured. It reserves one
// more index on a row that already carries depth records, with the verified prefix in place and
// with it disabled, so the two costs are reported side by side rather than argued about.
func BenchmarkStreamReserveAtDepth(b *testing.B) {
	for _, depth := range []int{1, 1000, 10000, 100000} {
		for _, prefix := range []bool{true, false} {
			name := fmt.Sprintf("depth=%d/verifiedPrefix=%v", depth, prefix)
			b.Run(name, func(b *testing.B) {
				dir := b.TempDir()
				store, err := OpenStreamStore(dir)
				if err != nil {
					b.Fatal(err)
				}
				defer store.Close()
				groupId := make([]byte, 32)
				senderHandle := make([]byte, 16)
				groupId[0] = 0x9a
				key, err := streamKeyFromOctets(groupId, senderHandle)
				if err != nil {
					b.Fatal(err)
				}
				rowName := streamRowName(key)
				body := make([]byte, 0, depth*streamRecordWidth)
				for index := 1; index <= depth; index += 1 {
					record := encodeStreamRecord(rowName, uint64(index))
					body = append(body, record[:]...)
				}
				if err := os.WriteFile(filepath.Join(store.rowDir, rowName), body, 0o600); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for range b.N {
					if !prefix {
						store.allocMutex.Lock()
						store.verified = map[string]streamRowVerification{}
						store.allocMutex.Unlock()
					}
					if _, err := store.ReserveStreamIndex(groupId, senderHandle); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkStreamHighWaterAtDepth isolates the REREAD from the flush. BenchmarkStreamReserveAtDepth
// above measures the whole allocation, which on NTFS is dominated by the forced flush -- several
// milliseconds, and noisy -- so the rescan the review found is only legible at the largest depth
// there. This one performs no write at all, so what it reports is the cost of answering a high
// water: constant with the verified prefix, linear in records ever written without it.
func BenchmarkStreamHighWaterAtDepth(b *testing.B) {
	for _, depth := range []int{1, 1000, 10000, 100000} {
		for _, prefix := range []bool{true, false} {
			b.Run(fmt.Sprintf("depth=%d/verifiedPrefix=%v", depth, prefix), func(b *testing.B) {
				dir := b.TempDir()
				store, err := OpenStreamStore(dir)
				if err != nil {
					b.Fatal(err)
				}
				defer store.Close()
				groupId := make([]byte, 32)
				senderHandle := make([]byte, 16)
				groupId[0] = 0x9b
				key, err := streamKeyFromOctets(groupId, senderHandle)
				if err != nil {
					b.Fatal(err)
				}
				rowName := streamRowName(key)
				body := make([]byte, 0, depth*streamRecordWidth)
				for index := 1; index <= depth; index += 1 {
					record := encodeStreamRecord(rowName, uint64(index))
					body = append(body, record[:]...)
				}
				if err := os.WriteFile(filepath.Join(store.rowDir, rowName), body, 0o600); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for range b.N {
					if !prefix {
						store.allocMutex.Lock()
						store.verified = map[string]streamRowVerification{}
						store.allocMutex.Unlock()
					}
					if _, err := store.StreamHighWater(groupId, senderHandle); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// ==============================================================================================
// Wave 3 -- two of the three ways an index could still be handed out twice. The third is the
// sentinel class the adapter's mapping must be total over, and it is in
// message_stream_adapter_test.go beside the mapping it gates.
// ==============================================================================================

// ----------------------------------------------------------------------------------------------
// Property A -- an index this store has RETURNED is inside the prefix it has CONFIRMED, at the
// moment it returns it.
// ----------------------------------------------------------------------------------------------

// THE CLAUSE THIS DRIVES IS writeOneRecord's verified-prefix assignment, AND IT DRIVES ITS
// DELETION rather than its position.
//
// Every mutation that clause had been put through before this MOVED it -- above the write, where
// an interrupted append then leaves the row shorter than the prefix the store claims, which
// TestAnInterruptedAppendLeavesTheRowAllocatableByTheNextCall catches. None DELETED it. Deleting
// it left the whole unfiltered root suite green at a1d55b1, and what it removes is the property
// this file exists for.
//
// WHY THE DELETION IS INVISIBLE EVERYWHERE ELSE. persistedHighWater re-reads the row on every
// call and re-seeds the prefix from what it read, so on the SECOND and every later allocation of
// a row the assignment is redundant -- the read has already put the prefix back. The one call it
// is not redundant on is the FIRST allocation of a row, because persistedHighWater took the
// absent-row exit, answered contract clause 4's error-free zero and recorded NOTHING. With the
// assignment gone the store returns index 1 having confirmed no part of the row it just created,
// and the rewind detector -- which is that prefix and there is no second one -- cannot see the row
// at all. Take the row away and the next call reads a stream never seen: index 1, a second time,
// under a class key that has not moved. Spec A section 5.6 calls a reused stream_index under a
// reused record_key "a total break of both AEADs for that record".
//
// Every existing case that covers a vanished or emptied row allocates THREE times first, or
// reopens the store, and both of those re-seed the prefix by another route. This one allocates
// EXACTLY ONCE, which is the state the deletion is visible in.
func TestTheIndexTheStoreJustReturnedIsInsideItsConfirmedPrefix(t *testing.T) {
	confirmedPrefix := func(t *testing.T, store *StreamStore, rowName string) (streamRowVerification, bool) {
		t.Helper()
		store.allocMutex.Lock()
		defer store.allocMutex.Unlock()
		prefix, seen := store.verified[rowName]
		return prefix, seen
	}

	t.Run("the prefix covers the index the first allocation returned", func(t *testing.T) {
		dir := t.TempDir()
		parts := streamTestKeyOctets(t, 0x71)
		rowName := streamTestRowName(t, parts)
		store := streamTestOpen(t, dir)

		index, err := store.ReserveStreamIndex(parts[0], parts[1])
		if err != nil || index != 1 {
			t.Fatalf("the first allocation answered (%d, %v), want (1, nil)", index, err)
		}
		prefix, seen := confirmedPrefix(t, store, rowName)
		if !seen {
			t.Fatalf("the store returned index %d for row %s and holds NO confirmed prefix for it; the confirmed prefix is the rewind detector and there is no second one, so a row it does not hold is a row that reads as a stream never seen the moment its bytes go away", index, rowName)
		}
		if prefix.highWater != index {
			t.Errorf("the store returned index %d and its confirmed prefix carries high water %d", index, prefix.highWater)
		}
		length := streamTestRowLength(t, filepath.Join(store.rowDir, rowName))
		if int64(prefix.records)*streamRecordWidth != length {
			t.Errorf("the confirmed prefix is %d record(s) = %d octets and the row is %d octets; a prefix narrower than the row it just wrote is a prefix a later read has to widen, and until it does the rewind detector is behind the disk",
				prefix.records, int64(prefix.records)*streamRecordWidth, length)
		}
	})

	t.Run("the row vanishes after exactly one allocation", func(t *testing.T) {
		dir := t.TempDir()
		parts := streamTestKeyOctets(t, 0x72)
		rowName := streamTestRowName(t, parts)
		store := streamTestOpen(t, dir)

		if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != 1 {
			t.Fatalf("the first allocation answered (%d, %v), want (1, nil)", index, err)
		}
		path := filepath.Join(store.rowDir, rowName)
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove the row: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the removal did not land: %v", err)
		}
		index, err := store.ReserveStreamIndex(parts[0], parts[1])
		if index == 1 {
			t.Fatalf("the allocation after the row vanished answered index 1 AGAIN (err %v); index 1 has already been returned to a caller for this key, and a second record under it is a reused nonce under a reused record_key", err)
		}
		if !errors.Is(err, ErrStreamStoreRewound) {
			t.Errorf("the allocation answered (%d, %v), want ErrStreamStoreRewound", index, err)
		}
		if !errors.Is(err, ErrStreamStoreConsumed) {
			t.Errorf("the allocation answered %v, which errors.Is does not find ErrStreamStoreConsumed in", err)
		}
		if index != 0 {
			t.Errorf("a refused allocation answered index %d beside its error", index)
		}
	})

	t.Run("the row is emptied in place after exactly one allocation", func(t *testing.T) {
		dir := t.TempDir()
		parts := streamTestKeyOctets(t, 0x73)
		rowName := streamTestRowName(t, parts)
		store := streamTestOpen(t, dir)

		if index, err := store.ReserveStreamIndex(parts[0], parts[1]); err != nil || index != 1 {
			t.Fatalf("the first allocation answered (%d, %v), want (1, nil)", index, err)
		}
		path := filepath.Join(store.rowDir, rowName)
		if err := os.Truncate(path, 0); err != nil {
			t.Fatalf("empty the row: %v", err)
		}
		if length := streamTestRowLength(t, path); length != 0 {
			t.Fatalf("the truncation did not land: the row is %d octets, want 0", length)
		}
		index, err := store.ReserveStreamIndex(parts[0], parts[1])
		if index == 1 {
			t.Fatalf("the allocation after the row was emptied answered index 1 AGAIN (err %v)", err)
		}
		if !errors.Is(err, ErrStreamStoreRewound) {
			t.Errorf("the allocation answered (%d, %v), want ErrStreamStoreRewound", index, err)
		}
		if !errors.Is(err, ErrStreamStoreConsumed) {
			t.Errorf("the allocation answered %v, which errors.Is does not find ErrStreamStoreConsumed in", err)
		}
	})
}

// ----------------------------------------------------------------------------------------------
// Property B -- the row classes the open-time seeding covers, and the complement it does not.
// ----------------------------------------------------------------------------------------------

// CLASS: every entry OpenStreamStore's scan can meet in the row directory, partitioned by the
// scan's OWN two decisions -- entry.Type().IsRegular(), then classifyStreamRowName -- and then by
// whether repairRow could classify the body. That is the partition written out above
// persistedHighWater, and this case is it, executable, WITH THE COMPLEMENT PRINTED.
//
// THE FINDING THIS CLOSES is class (5): a row of this build's key space whose BODY was corrupt at
// open. repairRow could not classify it, so before 2026-09-12 it entered nothing into the
// verified map and returned nil. Every other class is refused DIRECTORY-WIDE by rowDirectoryHolds
// -- a non-regular entry, a foreign tag, a name that is not a row -- so no key in the directory
// can allocate while one of them is present. Class (5) is the only one where the directory stays
// well-formed and every OTHER key goes on allocating normally, so the refusal has to be carried by
// the row's own entry in the verified map, and a row that is in no entry has no refusal to carry.
// Remove the corrupt row's bytes and it became a stream never seen: (0, nil), index 1, on a key
// whose row had durably carried indices.
//
// Mutation, re-measured in a disposable copy on 2026-09-12 because the round that keyed the
// permanence on the condition rather than on the clock CHANGED WHAT IT CATCHES, and the claim
// that was written here before that is now false. Delete
// `self.verified[rowName] = streamRowVerification{unreadable: true}` from repairRow and:
//
//	class (5) below goes red on its first assertion -- the scan entered nothing for the row;
//	TestTheUnreadableRowsPermanenceIsKeyedOnTheConditionAndNotOnWhenItWasFound goes red on its
//	  third seat, the store that never read the row before the bytes vanished, ALLOCATING index
//	  1 on a key whose row had durably carried indices;
//	TestACorruptRowIsPermanentThroughTheAdapterAndNotRetriedForever now PASSES, 200 of 200
//	  refused permanently. It used to be the loud one. It no longer is, because
//	  persistedHighWater marks the row on its own first READ of it, so by the time that case
//	  removes the bytes on attempt 3 the mark already exists. THE SCAN'S MARK IS STILL
//	  LOAD-BEARING, and what it is now the SOLE defence for is exactly the shape the third seat
//	  drives: a row whose octets go away before any call reads them, which a live read cannot
//	  mark because it never sees them.
func TestTheRowClassesTheOpenTimeSeedingCoversAndItsComplement(t *testing.T) {
	type rowClass struct {
		number    int
		name      string
		seeded    string
		refusedBy string
	}
	partition := []rowClass{
		{1, "an entry that is not a regular file", "NOT SEEDED", "rowDirectoryHolds, directory-wide, ErrStreamStoreState"},
		{2, "a row name of another key space", "NOT SEEDED", "rowDirectoryHolds, directory-wide, ErrStreamKeySpace"},
		{3, "a name that is not a row under any tag", "NOT SEEDED", "rowDirectoryHolds, directory-wide, ErrStreamStoreState"},
		{4, "this build's row, body classifies", "SEEDED with the confirmed prefix", "nothing -- this is the class the scan was written for"},
		{5, "this build's row, body does NOT classify", "SEEDED UNREADABLE", "persistedHighWater, this row only, ErrStreamStoreState with ErrStreamStoreConsumed"},
		{6, "this build's row, body cannot be READ", "NOT SEEDED -- no store is produced", "OpenStreamStore itself, ErrStreamStoreState"},
	}
	t.Log("THE PARTITION OpenStreamStore's SEEDING IS STATED OVER:")
	for _, class := range partition {
		t.Logf("  (%d) %-44s %-34s refused by: %s", class.number, class.name, class.seeded, class.refusedBy)
	}
	t.Log("COMPLEMENT of the class the seeding can READ -- classes (1), (2), (3), (5) and (6). (1), (2) and (3) are refused directory-wide, so no key allocates while one is present; (6) never produces a store at all; (5) is the one that leaves the directory well-formed, and it is the class this property is about")

	exercised := map[int]bool{}

	t.Run("class 1 -- an entry that is not a regular file", func(t *testing.T) {
		dir := t.TempDir()
		parts := streamTestKeyOctets(t, 0x74)
		if err := os.MkdirAll(filepath.Join(dir, streamRowDirName, "a-directory-is-not-a-row"), 0o700); err != nil {
			t.Fatalf("plant a directory in the row directory: %v", err)
		}
		store := streamTestOpen(t, dir)
		if _, err := store.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreState) {
			t.Errorf("the query answered %v, want ErrStreamStoreState", err)
		}
		if index, err := store.ReserveStreamIndex(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreState) || index != 0 {
			t.Errorf("the allocation answered (%d, %v), want (0, ErrStreamStoreState)", index, err)
		}
		exercised[1] = true
	})

	t.Run("class 2 -- a row of another key space", func(t *testing.T) {
		dir := t.TempDir()
		parts := streamTestKeyOctets(t, 0x75)
		preA1 := streamKeyPreA1{}
		copy(preA1.GroupId[:], parts[0])
		copy(preA1.SenderHandle[:], parts[1])
		foreign := streamRowNameOf(reflect.ValueOf(preA1))
		if strings.HasPrefix(foreign, streamKeySpaceTagOf(streamKeyType())) {
			t.Fatal("the pre-A1 field set derives this build's own tag, so this case cannot plant a foreign row")
		}
		streamTestPlantRow(t, dir, foreign, streamTestRowBody(foreign, 1, 2))
		store := streamTestOpen(t, dir)
		if _, err := store.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamKeySpace) {
			t.Errorf("the query answered %v, want ErrStreamKeySpace", err)
		}
		exercised[2] = true
	})

	t.Run("class 3 -- a name that is not a row under any tag", func(t *testing.T) {
		dir := t.TempDir()
		parts := streamTestKeyOctets(t, 0x76)
		streamTestPlantRow(t, dir, "this-is-not-a-row-name", []byte("nor is this a row body"))
		store := streamTestOpen(t, dir)
		if _, err := store.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreState) {
			t.Errorf("the query answered %v, want ErrStreamStoreState", err)
		}
		exercised[3] = true
	})

	t.Run("class 4 -- this build's row, body classifies", func(t *testing.T) {
		dir := t.TempDir()
		parts := streamTestKeyOctets(t, 0x77)
		rowName := streamTestRowName(t, parts)
		streamTestPlantRow(t, dir, rowName, streamTestRowBody(rowName, 1, 2, 3))
		store := streamTestOpen(t, dir)
		store.allocMutex.Lock()
		prefix, seen := store.verified[rowName]
		store.allocMutex.Unlock()
		if !seen || prefix.unreadable || prefix.records != 3 || prefix.highWater != 3 {
			t.Fatalf("the open-time scan seeded %+v (seen %v), want three records carrying high water 3 and not unreadable", prefix, seen)
		}
		if highWater, err := store.StreamHighWater(parts[0], parts[1]); err != nil || highWater != 3 {
			t.Errorf("the query answered (%d, %v), want (3, nil)", highWater, err)
		}
		exercised[4] = true
	})

	t.Run("class 5 -- this build's row, body does not classify", func(t *testing.T) {
		dir := t.TempDir()
		corrupt := streamTestKeyOctets(t, 0x78)
		corruptRow := streamTestRowName(t, corrupt)
		healthy := streamTestKeyOctets(t, 0x79)
		healthyRow := streamTestRowName(t, healthy)
		if corruptRow == healthyRow {
			t.Fatal("the two fixture keys name one row")
		}
		body := streamTestRowBody(corruptRow, 1, 2, 3)
		// record 2 fails with a verifying record 3 after it: a corrupt body, and not a
		// shape any interrupted append can leave.
		streamTestCorruptRecord(body, 2)
		corruptPath := streamTestPlantRow(t, dir, corruptRow, body)
		streamTestPlantRow(t, dir, healthyRow, streamTestRowBody(healthyRow, 1, 2, 3, 4))

		store := streamTestOpen(t, dir)
		store.allocMutex.Lock()
		prefix, seen := store.verified[corruptRow]
		store.allocMutex.Unlock()
		if !seen {
			t.Fatalf("the open-time scan entered NOTHING for a row it could not classify; a row that is in no entry is a row the rewind detector cannot see, and taking its bytes away then makes it a stream never seen")
		}
		if !prefix.unreadable {
			t.Errorf("the open-time scan seeded %+v for a row whose body did not classify, want the unreadable marker", prefix)
		}

		// it refuses while it is there, on both seats, and the refusal is PERMANENT rather
		// than the transient the whole ErrStreamStoreState class is otherwise ruled.
		for _, seat := range []struct {
			name string
			call func() (uint64, error)
		}{
			{"StreamHighWater", func() (uint64, error) { return store.StreamHighWater(corrupt[0], corrupt[1]) }},
			{"ReserveStreamIndex", func() (uint64, error) { return store.ReserveStreamIndex(corrupt[0], corrupt[1]) }},
		} {
			answer, err := seat.call()
			if !errors.Is(err, ErrStreamStoreState) {
				t.Errorf("%s answered (%d, %v) for a corrupt row, want ErrStreamStoreState", seat.name, answer, err)
			}
			if !errors.Is(err, ErrStreamStoreConsumed) {
				t.Errorf("%s answered %v, which errors.Is does not find ErrStreamStoreConsumed in; a store that could not read this row at open cannot read it later, because nothing in this store ever rewrites a row it refused, so a ratchet told to retry would ask forever and pay a durable write per attempt", seat.name, err)
			}
			if answer != 0 {
				t.Errorf("%s answered %d beside its error", seat.name, answer)
			}
		}

		// THE HOLE THIS CLOSES: the bytes go away and the refusal STAYS.
		if err := os.Remove(corruptPath); err != nil {
			t.Fatalf("remove the corrupt row: %v", err)
		}
		if _, err := os.Stat(corruptPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the removal did not land: %v", err)
		}
		for attempt := 1; attempt <= 3; attempt += 1 {
			index, err := store.ReserveStreamIndex(corrupt[0], corrupt[1])
			if err == nil {
				t.Fatalf("attempt %d ALLOCATED index %d on a row this store could not read at open and whose bytes have since been removed; that is contract clause 4's error-free zero answered for a row that durably carried indices, and the ladder restarts at 1 under a class key that has not moved", attempt, index)
			}
			if index != 0 {
				t.Errorf("attempt %d answered index %d beside its error", attempt, index)
			}
			if !errors.Is(err, ErrStreamStoreConsumed) {
				t.Errorf("attempt %d answered %v, which errors.Is does not find ErrStreamStoreConsumed in", attempt, err)
			}
		}
		if _, err := os.Stat(corruptPath); !errors.Is(err, os.ErrNotExist) {
			t.Error("a refusal recreated the row it refused")
		}

		// and the refusal is THIS ROW'S and not the directory's: every other key goes on
		// allocating, which is what separates class (5) from classes (1), (2) and (3).
		if highWater, err := store.StreamHighWater(healthy[0], healthy[1]); err != nil || highWater != 4 {
			t.Errorf("the healthy row in the same directory answered (%d, %v), want (4, nil); class (5) is refused per row and not directory-wide", highWater, err)
		}
		if index, err := store.ReserveStreamIndex(healthy[0], healthy[1]); err != nil || index != 5 {
			t.Errorf("the healthy row allocated (%d, %v), want (5, nil)", index, err)
		}

		// THE HORIZON, priced rather than hidden: the marker lives as long as this store,
		// exactly like the confirmed prefix it sits in. A store reopened over a directory
		// the corrupt row has been REMOVED from has nothing left to read and answers a
		// stream never seen. That is S2-23's residual -- nothing durable records what a
		// previous process confirmed -- and it is not widened by this field.
		if err := store.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		reopened := streamTestOpen(t, dir)
		if index, err := reopened.ReserveStreamIndex(corrupt[0], corrupt[1]); err != nil || index != 1 {
			t.Errorf("a store reopened over a directory the corrupt row was removed from answered (%d, %v); the residual this prices is that it answers (1, nil), so a change here is a change to that price and must be read as one", index, err)
		} else {
			t.Logf("RESIDUAL, S2-23: after a restart the removed corrupt row is indistinguishable from a row that never existed, so the reopened store allocates index %d for it. The unreadable marker's horizon is one store's lifetime, the same horizon the confirmed prefix has, and closing that would need a durable witness outside the row", index)
		}
		exercised[5] = true
	})

	t.Run("class 6 -- this build's row, body cannot be read", func(t *testing.T) {
		t.Log("NOT EXERCISED, and the reason is stated rather than left out: this class is repairRow's os.ReadFile failing on a REGULAR file whose name is one of this build's rows. The store carries no injected failure point on the open-time read -- streamAppendInterrupt is the allocation path's -- and there is no portable way to make a regular file unreadable to its owner on the GOOS set this ships to. What is known about it without running it: OpenStreamStore returns repairRow's error, so NO STORE IS PRODUCED and no index can be handed out on any key of that directory, which is a strictly stronger refusal than class (5)'s")
	})

	for _, class := range partition {
		if class.number == 6 {
			continue
		}
		if !exercised[class.number] {
			t.Errorf("class (%d) %q is in the partition and was not exercised, so this gate is not total over the partition it prints", class.number, class.name)
		}
	}
}

// ----------------------------------------------------------------------------------------------
// Property 15 -- an unreadable row's PERMANENCE is keyed on the condition and not on the clock.
// ----------------------------------------------------------------------------------------------

// THE TWO DISCOVERY ORDERS, DRIVEN AGAINST ONE ANOTHER RATHER THAN ASSERTED ONE AT A TIME.
//
// The defect this case exists for: streamRowVerification.unreadable was set in exactly one place,
// inside repairRow, at open, and its own words were "a row that was PRESENT in the row directory
// when this store opened". So THE SAME ROW, WITH THE SAME OCTETS, IN THE SAME DIRECTORY was
// classified two different ways depending on which of the store's two readers met it first: a
// corrupt body found at open carried ErrStreamStoreConsumed and stopped a ladder on attempt 1,
// and the identical body reaching persistedHighWater one call later under a live store was
// forwarded bare -- ErrStreamStoreState, which the adapter rules TRANSIENT, which is the
// unbounded retry paying a durable write per attempt against a row that will never accept one.
//
// A refusal is permanent because of what the store CAN'T DO about the row, not because of when it
// found out, and this case says that as an EQUALITY rather than as two separate assertions: two
// stores, two directories, ONE row body byte for byte, met in the two possible orders, and the
// same sentence out of both. An equality is what makes the property falsifiable by a one-sided
// change -- re-key either path on the clock and the strings stop matching, whichever way it is
// re-keyed.
//
// Mutation, measured in a disposable copy (unfiltered root suite, and it says so):
//
//	delete `self.verified[rowName] = streamRowVerification{unreadable: true}` and the
//	ErrStreamStoreConsumed wrap from persistedHighWater's classify-failure clause -- that is,
//	restore the bare `return 0, err` for a row not already marked -- and this case goes red on
//	the live store's first refusal, which carries no permanent sentinel at all.
func TestTheUnreadableRowsPermanenceIsKeyedOnTheConditionAndNotOnWhenItWasFound(t *testing.T) {
	parts := streamTestKeyOctets(t, 0x7a)
	rowName := streamTestRowName(t, parts)
	key, err := streamKeyFromOctets(parts...)
	if err != nil {
		t.Fatalf("build the stream key: %v", err)
	}
	// record 2 fails with a verifying record 3 after it: a corrupt body, and not a shape any
	// interrupted append can leave. One body, used twice.
	body := streamTestRowBody(rowName, 1, 2, 3)
	streamTestCorruptRecord(body, 2)

	// DISCOVERY AT OPEN: the row is already in the directory when the store opens, so
	// repairRow reads it and marks it.
	atOpenDir := t.TempDir()
	atOpenPath := streamTestPlantRow(t, atOpenDir, rowName, body)
	atOpen := streamTestOpen(t, atOpenDir)

	// DISCOVERY UNDER A LIVE STORE: the store opens over an empty directory, answers for the
	// key, and only then do the same octets appear under the same name. Nothing about the row
	// differs; only which of the store's two readers reached it first.
	liveDir := t.TempDir()
	live := streamTestOpen(t, liveDir)
	if highWater, err := live.StreamHighWater(parts[0], parts[1]); err != nil || highWater != 0 {
		t.Fatalf("the live store answered (%d, %v) before the row existed, want (0, nil)", highWater, err)
	}
	livePath := streamTestPlantRow(t, liveDir, rowName, body)

	atOpenBytes, err := os.ReadFile(atOpenPath)
	if err != nil {
		t.Fatalf("read the row the store opened over: %v", err)
	}
	liveBytes, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatalf("read the row that appeared under the live store: %v", err)
	}
	if !bytes.Equal(atOpenBytes, liveBytes) {
		t.Fatalf("the two rows are %d and %d octets and are not the same body, so this case is comparing two conditions and not two clocks", len(atOpenBytes), len(liveBytes))
	}
	t.Logf("ONE body, %d octets, under one name %s, in two directories. The only difference between the two stores is WHEN each first read it", len(atOpenBytes), rowName)

	type seat struct {
		name  string
		store *StreamStore
	}
	seats := []seat{{"found at open", atOpen}, {"found under a live store", live}}
	sentences := map[string]string{}
	for _, s := range seats {
		highWater, queryErr := s.store.StreamHighWater(parts[0], parts[1])
		if queryErr == nil {
			t.Fatalf("%s: the query answered (%d, nil) for a body that does not classify", s.name, highWater)
		}
		if highWater != 0 {
			t.Errorf("%s: the query answered high water %d beside its error", s.name, highWater)
		}
		if !errors.Is(queryErr, ErrStreamStoreState) {
			t.Errorf("%s: the refusal does not carry ErrStreamStoreState: %v", s.name, queryErr)
		}
		if !errors.Is(queryErr, ErrStreamStoreConsumed) {
			t.Errorf(
				"%s: the refusal carries no ErrStreamStoreConsumed, so the adapter rules it TRANSIENT and a SenderRatchet asks again -- without bound, paying a durable write per attempt, against a row that will never accept a record: %v",
				s.name, queryErr,
			)
		}
		index, reserveErr := s.store.ReserveStreamIndex(parts[0], parts[1])
		if reserveErr == nil {
			t.Fatalf("%s: the allocation handed out index %d on a row whose spent indices are not derivable from it", s.name, index)
		}
		if index != 0 {
			t.Errorf("%s: the allocation answered index %d beside its error", s.name, index)
		}
		if !errors.Is(reserveErr, ErrStreamStoreConsumed) {
			t.Errorf("%s: the allocation's refusal carries no ErrStreamStoreConsumed: %v", s.name, reserveErr)
		}
		sentences[s.name] = queryErr.Error()
		t.Logf("  %-26s %v", s.name, queryErr)
	}
	if sentences["found at open"] != sentences["found under a live store"] {
		t.Errorf(
			"the two discovery orders answer two different sentences for one body:\n  at open: %s\n  live:    %s\nA refusal that reads differently depending on when the store looked is a refusal keyed on the clock",
			sentences["found at open"], sentences["found under a live store"],
		)
	}

	// AND THE THIRD SEAT: A STORE THAT NEVER READ THE ROW AT ALL. It opened over the same
	// octets and the bytes were taken away before any call reached them, so the only record of
	// the row that ever existed is the one OpenStreamStore's scan made. This is the one shape
	// repairRow's own mark is the sole defence for -- persistedHighWater cannot mark a row
	// whose octets it never sees -- and it is driven here BEHAVIOURALLY rather than by reading
	// the verified map, so deleting that mark is caught by an answer and not only by a probe.
	neverReadDir := t.TempDir()
	neverReadPath := streamTestPlantRow(t, neverReadDir, rowName, body)
	neverRead := streamTestOpen(t, neverReadDir)
	if err := os.Remove(neverReadPath); err != nil {
		t.Fatalf("remove the row the store never read: %v", err)
	}
	if index, err := neverRead.ReserveStreamIndex(parts[0], parts[1]); err == nil {
		t.Errorf("a store that opened over a corrupt row and never read it before the bytes vanished answered index %d; the scan's own record is the only thing that stops that restarting the ladder at 1 on a key whose row had durably carried indices", index)
	} else if !errors.Is(err, ErrStreamStoreConsumed) {
		t.Errorf("the never-read store's refusal carries no ErrStreamStoreConsumed: %v", err)
	}

	// AND THE MARK OUTLIVES THE BYTES ON BOTH PATHS, which is the whole reason it is a mark
	// rather than a re-derivation: a row the store holds no record of is a row the rewind
	// detector cannot see, and removing it used to make it a stream never seen -- (0, nil),
	// and the next allocation hands out index 1 on a key that has durably spent it.
	for _, removal := range []struct {
		name string
		path string
	}{{"found at open", atOpenPath}, {"found under a live store", livePath}} {
		if err := os.Remove(removal.path); err != nil {
			t.Fatalf("%s: remove the corrupt row: %v", removal.name, err)
		}
	}
	for _, s := range seats {
		index, err := s.store.ReserveStreamIndex(parts[0], parts[1])
		if err == nil {
			t.Errorf("%s: after the corrupt row's bytes were removed the allocation answered index %d, restarting the ladder on a key whose row had durably carried indices", s.name, index)
			continue
		}
		if !errors.Is(err, ErrStreamStoreConsumed) {
			t.Errorf("%s: after the removal the refusal carries no ErrStreamStoreConsumed: %v", s.name, err)
		}
	}

	// and the whole of it through the production adapter, which is the seat SenderRatchet.Next
	// actually reads.
	for _, s := range seats {
		reserver := NewStreamIndexReserver(s.store)
		if index, err := reserver.Reserve(key); !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
			t.Errorf("%s: through the adapter the allocation answered (%d, %v), want messagegroup.ErrStreamIndexConsumed", s.name, index, err)
		}
		if highWater, err := reserver.HighWater(key); !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
			t.Errorf("%s: through the adapter the query answered (%d, %v), want messagegroup.ErrStreamIndexConsumed", s.name, highWater, err)
		}
	}
	t.Log("OCTETS OBTAINED AND REFUSED is permanent on both paths. The complement -- OCTETS NOT OBTAINED, an os.Open or a ReadAt that failed -- stays ErrStreamStoreState alone and stays a retry, and that is a condition and not a clock either")
}

// AND THE ONE EXIT FROM THE MARK THAT THE BYTES DO NOT REFUSE ON THEIR OWN.
//
// persistedHighWater's third `if prior.unreadable` clause is the row that CLASSIFIES NOW and did
// not when this store read it. Every other shape the mark has to survive is refused by something
// else -- the bytes refuse themselves while they are there, and the absent-row clause refuses them
// once they are gone -- so this is the only one whose whole defence is that clause. It was added
// by the round before this one with nothing driving it: replacing its condition with `false` left
// the entire unfiltered root suite at 264 pass / 0 fail, to the test, including every case written
// for the two sibling clauses ten lines above it.
//
// WHAT IT IS FOR. A row whose body did not classify has spent some number of indices and there is
// no way to learn which. Bytes that DO classify appearing in its place do not answer that
// question, they replace the evidence: the row's octets changed under the exclusion that makes
// this store the only writer, so re-seeding a high water from them is exactly the move that hands
// the next allocation a number this stream may already have used. The clause refuses instead, and
// it refuses WITHOUT writing the verified map, which is what keeps the mark from being laundered
// by the very call that met it.
//
// Mutations, each measured in a disposable copy against the unfiltered root suite:
//
//	replace the clause's condition with `false`         -> red here, on both discovery orders,
//	                                                       with the query answering (3, nil)
//	delete the clause entirely                          -> the same red
//	move the verified-map write above the clause         -> red on the stickiness assertion
func TestARowThatClassifiesAfterItDidNotIsRefusedRatherThanReSeeded(t *testing.T) {
	orders := []struct {
		name string
		open func(t *testing.T, dir string, rowName string, body []byte) *StreamStore
	}{
		{
			// the mark comes from repairRow, at open.
			name: "the row did not classify when the store opened",
			open: func(t *testing.T, dir string, rowName string, body []byte) *StreamStore {
				streamTestPlantRow(t, dir, rowName, body)
				return streamTestOpen(t, dir)
			},
		},
		{
			// the mark comes from persistedHighWater, under a live store. It exists at
			// all only because the permanence is keyed on the condition; before that it
			// was a transient refusal and left no mark to exit from.
			name: "the row did not classify when a live store read it",
			open: func(t *testing.T, dir string, rowName string, body []byte) *StreamStore {
				store := streamTestOpen(t, dir)
				streamTestPlantRow(t, dir, rowName, body)
				return store
			},
		},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			parts := streamTestKeyOctets(t, 0x7b)
			rowName := streamTestRowName(t, parts)
			dir := t.TempDir()
			corrupt := streamTestRowBody(rowName, 1, 2, 3)
			streamTestCorruptRecord(corrupt, 2)
			store := order.open(t, dir, rowName, corrupt)

			if _, err := store.StreamHighWater(parts[0], parts[1]); !errors.Is(err, ErrStreamStoreConsumed) {
				t.Fatalf("the row is not marked unreadable before the replacement: %v", err)
			}

			// THE REPLACEMENT: a perfectly well formed row, under the same name, in the
			// same directory. Nothing about these octets is wrong; what is wrong is that
			// they are not the octets this store refused.
			path := filepath.Join(dir, streamRowDirName, rowName)
			healthy := streamTestRowBody(rowName, 1, 2, 3)
			if err := os.WriteFile(path, healthy, 0o600); err != nil {
				t.Fatalf("replace the corrupt row with a well formed one: %v", err)
			}
			if _, _, err := classifyStreamRow(rowName, healthy); err != nil {
				t.Fatalf("the replacement body does not classify, so this case is not driving the clause it says it is: %v", err)
			}

			// THE STICKINESS IS PART OF THE PROPERTY: the clause returns without writing
			// the verified map, so a second call cannot read a prefix the first one left.
			const attempts = 3
			for attempt := 1; attempt <= attempts; attempt += 1 {
				highWater, err := store.StreamHighWater(parts[0], parts[1])
				if err == nil {
					t.Fatalf(
						"attempt %d: the query answered (%d, nil) by re-deriving a high water from octets that replaced the ones this store could not read; the indices the refused body had already spent are still not derivable, and allocating on the replacement hands out a number this stream may already have used",
						attempt, highWater,
					)
				}
				if highWater != 0 {
					t.Errorf("attempt %d: the query answered high water %d beside its error", attempt, highWater)
				}
				if !errors.Is(err, ErrStreamStoreState) || !errors.Is(err, ErrStreamStoreConsumed) {
					t.Errorf("attempt %d: the refusal is %v, want ErrStreamStoreState with ErrStreamStoreConsumed beside it", attempt, err)
				}
				if !strings.Contains(err.Error(), "classifies now") {
					t.Errorf("attempt %d: the refusal is %q and is not the one this clause writes, so this case is being satisfied by some other refusal", attempt, err)
				}
				index, err := store.ReserveStreamIndex(parts[0], parts[1])
				if err == nil {
					t.Fatalf("attempt %d: the allocation handed out index %d on the replacement body", attempt, index)
				}
				if index != 0 {
					t.Errorf("attempt %d: the allocation answered index %d beside its error", attempt, index)
				}
				if !errors.Is(err, ErrStreamStoreConsumed) {
					t.Errorf("attempt %d: the allocation's refusal carries no ErrStreamStoreConsumed: %v", attempt, err)
				}
			}

			// nothing was written, truncated, created or removed while the row was being
			// refused: the refusal is a refusal and not a repair.
			if length := streamTestRowLength(t, path); length != int64(len(healthy)) {
				t.Errorf("the replacement row is %d octets and was planted at %d; a refusal that rewrote the row would be the store rewriting a row it refused", length, len(healthy))
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the row back: %v", err)
			}
			if !bytes.Equal(after, healthy) {
				t.Errorf("the replacement row's octets changed under %d refusals", attempts)
			}
			entries, err := os.ReadDir(filepath.Join(dir, streamRowDirName))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("the row directory holds %d entries after the refusals, want exactly 1", len(entries))
			}

			// and through the production adapter, which is the seat that decides whether a
			// ladder stops or retries.
			key, err := streamKeyFromOctets(parts...)
			if err != nil {
				t.Fatalf("build the stream key: %v", err)
			}
			reserver := NewStreamIndexReserver(store)
			if index, err := reserver.Reserve(key); !errors.Is(err, messagegroup.ErrStreamIndexConsumed) {
				t.Errorf("through the adapter the allocation answered (%d, %v), want messagegroup.ErrStreamIndexConsumed", index, err)
			}
			t.Logf("%d refusals, none of them a repair, and the well formed replacement never became a high water", attempts)
		})
	}
}
