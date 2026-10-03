// S2-26, THE DEVICE X-WING PRIVATE HALF: the unit half of ledger item 243's first step.
//
// WHAT WAS WRONG, precisely, and it is one line. `deviceIdentity` drew an X-Wing key pair, encoded
// the PUBLIC half into the urmessage_leaf_keys extension (type 0xF002, alg 0x0014, 1216 octets)
// and let the private half go out of scope in the same statement. That extension travels: it is in
// every key package this device publishes, it is in the leaf every group's ratchet tree holds for
// it, and `connect/messagegroup`'s engine parses and re-encodes it on three paths. So the device
// advertised a wrap target and could not read one. `XwingEncapsulate` and `XwingDecapsulate` had
// ZERO production callers outside `connect/messagegroup/xwing.go` itself, which is the same fact
// from the other end.
//
// WHY IT IS WORTH A STEP OF ITS OWN. Item 243 rotates `pq_secret` per epoch and ruling 36 fixed
// the carrier as the X-Wing device wrap -- because HPKE in `connect/mls` is hard-wired to X25519
// (`hpke.go:214-247` and `:271-286` call X25519GenerateKey/X25519DH; `KemId` is a registry label
// the implementation never dispatches on), so the MLS exporter carries no post-quantum
// contribution at all and no cheaper shape can discharge a post-quantum gate. Every later step
// stands on a device being able to open what is addressed to it, and until this landed none could.
//
// THE CASES HERE ARE THE ONES THAT DO NOT NEED A SERVER. The property itself -- a real group, a
// real member's extension as it travels, that member's own device opening it -- is
// `cp3b/devicewrap_test.go`, because it needs the whole seam.
package urmessage

import (
	"bytes"
	"crypto/rand"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
)

// mintLeafKeys is one X-Wing key pair, as `deviceIdentity` mints one: the encoded leaf keys body
// and the seed under the public half inside it.
func mintLeafKeys(t *testing.T) (leafKeys []byte, seed []byte) {
	t.Helper()
	xwing, err := messagegroup.XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("messagegroup.XwingGenerateKey: %v", err)
	}
	encoded, err := (&mls.LeafKeysExtension{
		AlgId:          mls.AlgIdXwing,
		DeviceXwingPub: xwing.Public().Bytes(),
	}).Encode()
	if err != nil {
		t.Fatalf("encoding the leaf keys extension: %v", err)
	}
	return encoded.ExtensionData, xwing.Seed()
}

// THE SEED THIS DEVICE KEEPS IS THE SEED UNDER THE KEY IT PUBLISHES.
//
// This is the clause that cannot be met by keeping A seed: it has to be the seed of the pair whose
// public half went into the leaf keys body, and the two are separated by three statements in
// `deviceIdentity` and by one record on the disk. A refactor that drew the key twice -- once for
// the extension, once for the store -- produces a device that publishes one key, holds the seed of
// another, opens nothing and reports nothing, and every other case in this file passes.
func TestTheSeedADeviceKeepsExpandsToTheKeyItsLeafPublishes(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	crypto, err := mls.NewCryptoProvider(deviceCipherSuite)
	if err != nil {
		t.Fatalf("the mls crypto provider: %v", err)
	}
	_, _, leafKeys, seed, err := deviceIdentity(crypto, store, rand.Reader)
	if err != nil {
		t.Fatalf("deviceIdentity: %v", err)
	}
	if len(seed) != messagegroup.XwingSeedSize {
		t.Fatalf("a minted device kept a %d octet seed, want %d", len(seed), messagegroup.XwingSeedSize)
	}
	published, err := mls.ParseLeafKeysExtension(leafKeys)
	if err != nil {
		t.Fatalf("parsing the minted leaf keys body: %v", err)
	}
	expanded, err := messagegroup.XwingKeyGenFromSeed(seed)
	if err != nil {
		t.Fatalf("expanding the kept seed: %v", err)
	}
	if !bytes.Equal(expanded.Public().Bytes(), published.DeviceXwingPub) {
		t.Fatalf("the kept seed expands to %x... and the leaf publishes %x...; they are two different key pairs",
			expanded.Public().Bytes()[:16], published.DeviceXwingPub[:16])
	}

	// AND IT IS THE SAME PAIR AFTER A RESTART, off the disk and not off this process. A second
	// deviceIdentity over the same store is what the next process does.
	_, _, againKeys, againSeed, err := deviceIdentity(crypto, store, rand.Reader)
	if err != nil {
		t.Fatalf("the second deviceIdentity: %v", err)
	}
	if !bytes.Equal(againKeys, leafKeys) {
		t.Errorf("the restored leaf keys body is not the one that was minted")
	}
	if !bytes.Equal(againSeed, seed) {
		t.Errorf("the restored seed is not the one that was minted")
	}
}

// A STORE WRITTEN BEFORE THE SEED EXISTED IS NOT A DEVICE THAT CANNOT START.
//
// The deployed alpha's directory is one of these: a THREE part device identity record. This case
// drives exactly it, and asserts the three things that together are the backward compatibility
// answer.
//
//  1. The read answers a nil error and an EMPTY seed. A refusal here is a device that can never
//     start again, and this store's one version lever is read for every record in the directory,
//     so it could not have been spent on this field alone.
//  2. THE LEAF KEYS BODY IS THE ONE THAT WAS THERE. No re-mint. The leaf every group's ratchet
//     tree holds for this device carries the OLD public half, so a fresh pair would open nothing
//     either -- and would turn a refusal that names its cause into a decapsulation that answers a
//     wrong secret. This is the clause a "helpful" repair would break.
//  3. The one thing such a device cannot do refuses BY NAME.
//
// THE POSITIVE CONTROL IS INLINE AND IS THE SECOND HALF OF THE SAME CASE: the identical
// construction over a store that HOLDS a seed opens the identical ciphertext. Without it, clause 3
// passes against a build where DecapsulateToOwnLeaf refuses everything.
func TestAStoreWrittenBeforeTheWrapSeedRestoresWithItsOwnLeafAndRefusesByName(t *testing.T) {
	crypto, err := mls.NewCryptoProvider(deviceCipherSuite)
	if err != nil {
		t.Fatalf("the mls crypto provider: %v", err)
	}
	leafKeys, seed := mintLeafKeys(t)
	published, err := mls.ParseLeafKeysExtension(leafKeys)
	if err != nil {
		t.Fatalf("parsing the leaf keys body: %v", err)
	}
	pub, err := messagegroup.ParseXwingPublicKey(published.DeviceXwingPub)
	if err != nil {
		t.Fatalf("parsing the published encapsulation key: %v", err)
	}
	ciphertext, shared, err := messagegroup.XwingEncapsulate(rand.Reader, pub)
	if err != nil {
		t.Fatalf("messagegroup.XwingEncapsulate: %v", err)
	}

	// ── the old store: the record a build before S2-26 wrote ──────────────────────────────
	old := openTestStore(t, t.TempDir())
	old.lock.Lock()
	writeErr := old.writeRecord(old.identityPath(), stateKindDeviceIdentity, testPub, testPriv, leafKeys)
	old.lock.Unlock()
	if writeErr != nil {
		t.Fatalf("writing a three part device identity: %v", writeErr)
	}
	gotPub, gotPriv, gotKeys, gotSeed, err := old.GetDeviceIdentity()
	if err != nil {
		t.Fatalf("(1) a three part device identity was refused: %v", err)
	}
	if len(gotSeed) != 0 {
		t.Errorf("(1) a store written before the seed answered a %d octet seed", len(gotSeed))
	}
	if !bytes.Equal(gotPub, testPub) || !bytes.Equal(gotPriv, testPriv) {
		t.Errorf("(1) the old store's signature key pair did not come back")
	}
	if !bytes.Equal(gotKeys, leafKeys) {
		t.Errorf("(1) the old store's leaf keys body did not come back")
	}

	_, _, throughIdentity, noSeed, err := deviceIdentity(crypto, old, rand.Reader)
	if err != nil {
		t.Fatalf("(2) deviceIdentity over a store written before the seed: %v", err)
	}
	if len(noSeed) != 0 {
		t.Errorf("(2) deviceIdentity answered a %d octet seed over a store that holds none", len(noSeed))
	}
	if !bytes.Equal(throughIdentity, leafKeys) {
		t.Fatal("(2) deviceIdentity MINTED OVER the old store's leaf keys body; the leaf every group holds for this device carries the old public half, so the new pair opens nothing and the refusal that names the absence is gone")
	}

	dark := &Device{wrapSeed: noSeed, groups: map[string]*Group{}}
	if _, err := dark.DecapsulateToOwnLeaf(ciphertext); !errors.Is(err, ErrNoDeviceWrapKey) {
		t.Errorf("(3) a device restored from a store with no seed answered %v, want ErrNoDeviceWrapKey", err)
	}

	// ── the control, and it fires for its own reason: the same construction WITH a seed ───
	lit := &Device{wrapSeed: seed, groups: map[string]*Group{}}
	opened, err := lit.DecapsulateToOwnLeaf(ciphertext)
	if err != nil {
		t.Fatalf("the control: a device holding the seed answered %v", err)
	}
	if !bytes.Equal(opened, shared) {
		t.Fatalf("the control: a device holding the seed opened the encapsulation to a different secret, so clause 3 above is measuring nothing")
	}
}

// THE SEED IS CHECKED AT THE WRITE, WHERE THE VALUE IS STILL IN THE CALLER'S HAND.
//
// A write with no seed puts the directory back in exactly the state this field was added to leave,
// and does it with a nil error. A seed of the wrong length is worse than useless rather than
// merely wrong: 32 and 64 both expand into a well formed X-Wing key pair -- which is the hazard
// `messagegroup`'s own constants are named apart for -- so only one of them is the pair whose
// public half is in the leaf keys body beside it.
//
// THE LAST ROW IS THE POSITIVE CONTROL AND IT IS IN THE SAME TABLE: the real length is accepted,
// so a refusal above it is this check and not a store that refuses every identity.
func TestPutDeviceIdentityRefusesEveryWrapSeedButARealOne(t *testing.T) {
	for _, one := range []struct {
		name    string
		seed    []byte
		refused bool
	}{
		{"none at all", nil, true},
		{"empty", []byte{}, true},
		{"one short", bytes.Repeat([]byte{0x11}, messagegroup.XwingSeedSize-1), true},
		{"one long", bytes.Repeat([]byte{0x11}, messagegroup.XwingSeedSize+1), true},
		{"the expanded ml-kem seed", bytes.Repeat([]byte{0x11}, messagegroup.XwingMlkemSeedSize), true},
		{"a real seed", bytes.Repeat([]byte{0x11}, messagegroup.XwingSeedSize), false},
	} {
		store := openTestStore(t, t.TempDir())
		err := store.PutDeviceIdentity(testPub, testPriv, testKp, one.seed)
		switch {
		case one.refused && !errors.Is(err, ErrStateStoreFormat):
			t.Errorf("PutDeviceIdentity with %s answered %v, want ErrStateStoreFormat", one.name, err)
		case !one.refused && err != nil:
			t.Errorf("PutDeviceIdentity with %s answered %v, want it written", one.name, err)
		}
	}
}

// A STORED SEED OF THE WRONG LENGTH IS REFUSED AT THE READ TOO, and it is a DIFFERENT case from
// the three part record above: four parts means the writer meant to store a seed, so a fourth part
// that is not one is a record this build cannot act on rather than a store from before the field.
// Reading it as a seed would hand XwingKeyGenFromSeed something it refuses at the next
// decapsulation, at a site with nothing to point at.
//
// THE CONTROL IS THE SAME WRITE AT THE REAL LENGTH, through the same unexported path.
func TestAStoredWrapSeedThatIsNotASeedIsRefusedAtTheRead(t *testing.T) {
	for _, one := range []struct {
		name    string
		seed    []byte
		refused bool
	}{
		{"one short", bytes.Repeat([]byte{0x44}, messagegroup.XwingSeedSize-1), true},
		{"the expanded ml-kem seed", bytes.Repeat([]byte{0x44}, messagegroup.XwingMlkemSeedSize), true},
		{"empty", []byte{}, true},
		{"a real seed", bytes.Repeat([]byte{0x44}, messagegroup.XwingSeedSize), false},
	} {
		store := openTestStore(t, t.TempDir())
		store.lock.Lock()
		writeErr := store.writeRecord(store.identityPath(), stateKindDeviceIdentity,
			testPub, testPriv, testKp, one.seed)
		store.lock.Unlock()
		if writeErr != nil {
			t.Fatalf("writing a four part device identity with %s: %v", one.name, writeErr)
		}
		_, _, _, seed, err := store.GetDeviceIdentity()
		switch {
		case one.refused && !errors.Is(err, ErrStateStoreFormat):
			t.Errorf("GetDeviceIdentity over %s answered %v, want ErrStateStoreFormat", one.name, err)
		case !one.refused && err != nil:
			t.Errorf("GetDeviceIdentity over %s answered %v, want the seed", one.name, err)
		case !one.refused && !bytes.Equal(seed, one.seed):
			t.Errorf("GetDeviceIdentity over %s answered %x", one.name, seed)
		}
	}
}

// CLOSING A DEVICE ERASES THE SEED, MEASURED ON THE ARRAY AND NOT ON THE FIELD.
//
// The alias is taken BEFORE the Close and read AFTER it, so a Close that set the field to nil and
// left the octets where they were fails here. That distinction is the whole case: nil-ing a field
// is what a reader assumes happened and is not an erase.
//
// THE CONTROL IS THE SAME ALIAS BEFORE THE CLOSE. A case that asserted "all zero" over an array
// that was already all zero would pass against a device that never held a seed at all.
func TestClosingADeviceErasesTheSeedAndNotOnlyTheField(t *testing.T) {
	_, seed := mintLeafKeys(t)
	device := &Device{wrapSeed: seed, groups: map[string]*Group{}}
	alias := device.wrapSeed

	nonZero := false
	for _, octet := range alias {
		if octet != 0 {
			nonZero = true
		}
	}
	if !nonZero {
		t.Fatal("the control: the seed was already all zero before the Close, so the assertion below measures nothing")
	}

	if err := device.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for at, octet := range alias {
		if octet != 0 {
			t.Fatalf("octet %d of the seed survived the Close; the field was dropped and the key material was not", at)
		}
	}
	if device.wrapSeed != nil {
		t.Error("the field still points at the erased array; a decapsulation after a Close would run under 32 zero octets, which is a valid seed")
	}
	if _, err := device.DecapsulateToOwnLeaf(make([]byte, messagegroup.XwingCiphertextSize)); !errors.Is(err, ErrNoDeviceWrapKey) {
		t.Errorf("a closed device answered %v, want ErrNoDeviceWrapKey", err)
	}
	if err := device.Close(); err != nil {
		t.Errorf("a second Close answered %v; it is meant to be idempotent", err)
	}
}

// NO TYPE IN THIS PACKAGE HOLDS A *messagegroup.XwingPrivateKey IN A FIELD.
//
// THIS GATE IS A SENTENCE ANOTHER REPOSITORY'S GATE ASSERTS AND CANNOT SEE HERE.
// `connect/mls/staged_erase_test.go` excuses `XwingPrivateKey` from owing an erase with a written
// reason: *"an ANSWER. XwingGenerateKey and XwingKeyGenFromSeed build one per call and no
// production declaration holds one in a field; the seed inside it is the caller's to keep or to
// drop."* Its scan roots are `{".", "../message", "../messagegroup"}` -- so `sdk` is outside them,
// and the day this package put one in a field that excuse would be false in a tree nothing checks,
// over a type that declares no Zeroize and that this package could not erase if it wanted to.
//
// Holding the 32 octet SEED instead is what keeps the sentence true and what makes
// [Device.Close]'s erase possible at all, and this is the check that keeps it that way rather than
// a comment saying so.
//
// THE POSITIVE CONTROL IS INLINE AND IT NAMES A FIELD THE WALK MUST HAVE SEEN. A parse that read
// nothing -- a moved file, a changed package name, a filter that excludes everything -- reports no
// XwingPrivateKey field for the same reason a correct tree does. So the walk must also FIND
// `Device.wrapSeed`, and the refusal above is vacuous without it.
func TestNoFieldInThisPackageHoldsAnUnerasableXwingPrivateKey(t *testing.T) {
	fileSet := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	sawTheSeed := false
	fieldsRead := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			typeSpec, isType := node.(*ast.TypeSpec)
			if !isType {
				return true
			}
			structType, isStruct := typeSpec.Type.(*ast.StructType)
			if !isStruct || structType.Fields == nil {
				return true
			}
			for _, field := range structType.Fields.List {
				fieldsRead += 1
				spelling := types.ExprString(field.Type)
				for _, fieldName := range field.Names {
					if typeSpec.Name.Name == "Device" && fieldName.Name == "wrapSeed" {
						sawTheSeed = true
						if spelling != "[]byte" {
							t.Errorf("Device.wrapSeed is %s; it is held as the 32 octet seed so that zeroizeState can erase it, and %s is not octets this package can erase",
								spelling, spelling)
						}
					}
					if strings.Contains(spelling, "XwingPrivateKey") {
						t.Errorf("%s.%s is %s: connect/mls's erase gate excuses XwingPrivateKey from owing an erase on the ground that no production declaration holds one in a field, its scan roots do not reach sdk, and nothing in this package can erase one. Hold the seed.",
							typeSpec.Name.Name, fieldName.Name, spelling)
					}
				}
			}
			return true
		})
	}
	if fieldsRead == 0 {
		t.Fatal("the walk read no struct field at all, so it refused nothing for the same reason a clean package would")
	}
	if !sawTheSeed {
		t.Fatalf("the walk read %d struct fields and none of them is Device.wrapSeed; it is not reading this package's production source and its refusal above is vacuous",
			fieldsRead)
	}
	t.Logf("read %d struct fields of package urmessage's production source", fieldsRead)
}
