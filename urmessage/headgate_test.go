package urmessage

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
)

// ── invariant H1, as a gate rather than a paragraph ──────────────────────────────────────────

// keySourceAeadTagBytes' local twin: chacha20poly1305.Overhead, which connect names
// recordAeadTagBytes at messagegroup/recordaead.go:67. It is written here as the number the codec
// derives rather than imported, because the derivation is the point: 9 octets of head plus 16 of
// tag is 25 octets of ct_head, which is what the live table holds on all 3,758 rows.
const headAeadTagBytes = 16

// EVERY RECORD THIS BUILD SEALS CARRIES A 25 OCTET ct_head, ON ALL FOUR RECORD KINDS.
//
// WHAT THIS CATCHES AND WHEN, which is how a gate is judged. It removes "a record whose head is not
// 9 octets" from the set of records this build can produce, and it catches on the SEAL side at TEST
// TIME -- before anything ships, not on receipt and not in production. The complement it removes is
// non-empty and is the whole point: any future head field, any kind-dependent head width, any
// second head layout.
//
// WHY THAT COMPLEMENT MATTERS MORE THAN AN OCTET. ct_body's width is QUANTISED and the server
// ENFORCES the equality -- octet_length(ct_body) = size_bucket_bytes[b] + 16, spec B §5.1 check 3,
// msgrepo/api/submit.go:299-306 -- so a body is length-opaque by construction. ct_head is not: it
// travels as a bare WriteOpaqueLP (connect/message/codec.go:155) bounded only by a 64 KiB cap, so
// it is length-TRANSPARENT by construction. A head whose presence or width depends on what a record
// SAYS therefore leaks that to the server, permanently, on every PERMANENT and DURABLE row already
// stored -- ct_head is erased only for EPH(1..5) -- and no later change un-leaks them. That is the
// one irreversible thing in the 2026-09-17 ruling, and this case is what keeps the door shut by
// RULING rather than by accident.
//
// THE FOUR KINDS ARE THE FOUR SEAL SITES: the founding commit (group.go, Open step 1), the device
// wrap (step 2), the epoch-complete marker (step 3) and the application record
// ([Group.sendContentLocked]). They are sealed here the way those four sites seal them --
// TestTheFourSealSitesAllPassEncodeHead is what holds this case's four against the file's four, so
// that a fifth site, or a site that stopped passing encodeHead, is red rather than uncovered.
//
// keysource_test.go's TestEveryKeyedOctetOfARecordIsReproducibleFromTheExporterAndTheTwoInjectedValuesAlone
// is the assertion shape this follows, one repo over.
func TestEveryRecordThisBuildSealsCarriesA25OctetCtHead(t *testing.T) {
	sealed := sealOneOfEveryRecordKind(t)
	if len(sealed) != 4 {
		t.Fatalf("this case sealed %d record kinds and the build has four", len(sealed))
	}
	for _, one := range sealed {
		if len(one.record.CtHead) != headBytes+headAeadTagBytes {
			t.Errorf("%s: ct_head is %d octets, want %d (%d octets of head plus a %d octet tag)",
				one.name, len(one.record.CtHead), headBytes+headAeadTagBytes, headBytes, headAeadTagBytes)
		}
		t.Logf("%s: ct_head %d octets, ct_body %d octets, class %d",
			one.name, len(one.record.CtHead), len(one.record.CtBody), one.record.Header.RetentionClass)
	}
	// AND THE WIDTH IS THE ONE THE CODEC DERIVES, not a constant this file chose. A headBytes
	// that moved with this number moved with it would be two copies of one mistake.
	if headBytes != 1+8 {
		t.Errorf("headBytes is %d; the head is version ‖ sent_at and is frozen at 9 octets", headBytes)
	}
	if headVersion != 0x02 {
		t.Errorf("headVersion is 0x%02x; the content envelope announces itself as 0x02", headVersion)
	}
}

// EVERY SEAL SITE PASSES encodeHead, AND THERE ARE SEVEN OF THEM ACROSS THE FOUR KINDS.
//
// The case above seals its own four records, which makes it a statement about encodeHead's width
// and not yet a statement about the SOURCE. This is the other half: every SealRecord call this
// package makes hands encodeHead's answer as its head argument. A site that built a head some other
// way would leave the case above green over a build that seals a head it never measured.
//
// IT COUNTS SITES AND THE COUNT IS SEVEN, NOT FOUR, SINCE THE SECOND EPOCH LANDED. There are still
// only four record KINDS -- the case above seals one of each -- but a group now publishes those
// kinds from TWO sets of sites: [Group.Open] founds epoch one (the founding commit, a wrap, the
// marker), and [Group.AddMemberAndPublish] with [Group.publishEpochFanoutLocked] opens every epoch
// after it (an epoch commit, a wrap, the marker), plus the one application site in
// [Group.sendContentLocked]. Three of the seven are new sites of three kinds this gate already
// covers, not new kinds; what matters is that each still passes encodeHead, which is what the loop
// below checks on all seven. A site added or one that stopped passing encodeHead is red rather than
// uncovered.
//
// IT MEASURES THE CALL AND NOT THE NAME, and it used to measure the name. The old body asked whether
// `call.Args[3].(*ast.CallExpr).Fun` was an [ast.Ident] spelled "encodeHead" -- which is a question
// about nine characters of text. A package-level `var encodeHead = func(int64) []byte { ... }`, or a
// local of that name in scope at the seal site, or a method reached through a receiver that happened
// to be spelled the same, all satisfy it while sealing a head nothing in this suite has measured.
// The identifier is now RESOLVED, through go/types, and held against the one object
// [encodeHead] names in this package's scope: not a name that matches, THE FUNCTION.
//
// THE TYPE-CHECK RUNS WITH AN IMPORTER THAT REFUSES EVERYTHING, and that is a decision rather than a
// shortcut. What this gate needs to resolve is a package-level function declared in a file it is
// already reading, and nothing about connect, protobuf or the standard library bears on it -- so
// the imports are allowed to fail (41 errors, swallowed) and the check still binds every use of
// encodeHead to record.go's declaration. It costs 13ms. An importer that actually resolved the
// imports would cost 3.5s per run and would make this gate fail on a machine where a dependency
// does not build, which is a gate measuring the environment.
//
// AND IT READS EVERY PRODUCTION FILE, NOT group.go. It scanned one file, so a seal site added in a
// new file was not a fifth site this gate could see -- it was no site at all, and the count stayed
// at four. [stateTestProductionSources] is the package's own enumerator and it is what
// TestEveryFsyncInThisPackageIsAtASiteThisSuiteNames already holds its own count against.
func TestTheFourSealSitesAllPassEncodeHead(t *testing.T) {
	fileSet, files := headGateParsePackage(t)
	pkg, uses := headGateTypeCheck(t, fileSet, files)

	// THE OBJECT EVERY SITE IS HELD AGAINST. It is looked up rather than assumed so that the
	// failure "there is no such function any more" reads as itself rather than as four site
	// failures.
	declared := pkg.Scope().Lookup("encodeHead")
	if declared == nil {
		t.Fatal("this package's scope holds no encodeHead, so there is nothing for a seal site to pass")
	}
	asFunc, isFunc := declared.(*types.Func)
	if !isFunc {
		t.Fatalf("encodeHead resolves to %T and not to a function; a head built by a value a caller can rebind is a head this suite does not measure", declared)
	}
	if asFunc.Signature().Recv() != nil {
		t.Fatal("encodeHead has a receiver, so which head it builds depends on what it is called on")
	}
	t.Logf("encodeHead resolves to %v, declared at %s", asFunc, fileSet.Position(asFunc.Pos()))

	sites := []string{}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector || selector.Sel.Name != "SealRecord" {
				return true
			}
			where := fileSet.Position(call.Pos()).String()
			sites = append(sites, where)
			if len(call.Args) != 7 {
				t.Errorf("%s: SealRecord takes 7 arguments and this call passes %d", where, len(call.Args))
				return true
			}
			head, isCall := call.Args[3].(*ast.CallExpr)
			if !isCall {
				t.Errorf("%s: the head argument is not a call, so it is not encodeHead's answer", where)
				return true
			}
			callee := headGateCallee(head.Fun)
			if callee == nil {
				t.Errorf("%s: the head argument is a call through %T, which names no identifier this gate can resolve -- so what builds this record's head is not something this suite measured",
					where, head.Fun)
				return true
			}
			if resolved := uses[callee]; resolved != declared {
				t.Errorf("%s: the head argument calls %q, which resolves to %v declared at %s -- and the function this suite measures is %v declared at %s. This record's head is built somewhere this suite does not measure",
					where, callee.Name, resolved, headGatePositionOf(fileSet, resolved),
					asFunc, fileSet.Position(asFunc.Pos()))
			}
			return true
		})
	}
	if len(sites) != 7 {
		t.Errorf("this package holds %d SealRecord call(s); the build has four record kinds published from seven sites (Open founds epoch one, AddMemberAndPublish opens every epoch after, plus the one application site): %v", len(sites), sites)
	}
	t.Logf("the seven seal sites: %v", sites)
}

// headGateParsePackage parses every production source in this package, WHATEVER GOOS IT IS
// CONSTRAINED TO -- see [stateTestProductionSources] for why that is the right set to read and not
// the set this build compiles.
//
// THE THREE PLATFORM FILES REDECLARE ONE ANOTHER and the type-check below says so, loudly, in the
// errors it swallows. That is harmless here and is worth stating rather than discovering: the
// duplicates are syncStateDir and its neighbours, this gate resolves encodeHead, and the two do not
// meet. A gate that dropped the platform files to quiet the checker would be a gate that stopped
// reading three of this package's files to make itself easier to write.
func headGateParsePackage(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, name := range stateTestProductionSources(t) {
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files = append(files, parsed)
	}
	return fileSet, files
}

// headGateTypeCheck binds every identifier in those files to the object it names, as far as a
// checker with no imports can. See TestTheFourSealSitesAllPassEncodeHead for why no imports.
func headGateTypeCheck(t *testing.T, fileSet *token.FileSet, files []*ast.File) (*types.Package, map[*ast.Ident]types.Object) {
	t.Helper()
	swallowed := 0
	config := &types.Config{
		Importer:                 headGateNoImports{},
		Error:                    func(error) { swallowed += 1 },
		DisableUnusedImportCheck: true,
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	pkg, _ := config.Check("github.com/urnetwork/sdk/urmessage", fileSet, files, info)
	if pkg == nil {
		t.Fatal("the type-check produced no package, so nothing below resolves")
	}
	t.Logf("type-checked %d file(s), %d error(s) swallowed (the refused imports and the platform redeclarations)",
		len(files), swallowed)
	return pkg, info.Uses
}

// headGateNoImports refuses every import. The package under the checker is this one, and the only
// object this gate resolves is declared inside it.
type headGateNoImports struct{}

func (headGateNoImports) Import(path string) (*types.Package, error) {
	return nil, fmt.Errorf("this gate resolves no imports, and %s is one", path)
}

// headGateCallee is the identifier a call expression names, for the two shapes a call to a function
// can take: `f(...)` and `x.f(...)`. Anything else -- a call through a returned value, an index, a
// conversion -- answers nil, which the caller reports as a head built through an expression this
// suite cannot follow. That is the honest answer and not a pass.
func headGateCallee(fun ast.Expr) *ast.Ident {
	switch named := fun.(type) {
	case *ast.Ident:
		return named
	case *ast.SelectorExpr:
		return named.Sel
	}
	return nil
}

func headGatePositionOf(fileSet *token.FileSet, object types.Object) string {
	if object == nil {
		return "nowhere this gate could resolve"
	}
	return fileSet.Position(object.Pos()).String()
}

// ── one of every record kind, sealed the way the build seals it ──────────────────────────────

type sealedKind struct {
	name   string
	record *message.Record
}

// sealOneOfEveryRecordKind founds a group the way [Device.CreateGroup], [Group.AddMember] and
// [Group.Open] found one, and seals one record of each of the four kinds through the same sessions
// at the same classes.
//
// IT DOES NOT GO THROUGH [Group.Open] and it cannot: Open submits, and a submit needs a server,
// which is what the cp3b module is for. What it needs from Open is the SEAL ARGUMENTS, and those
// are held against the file by TestTheFourSealSitesAllPassEncodeHead.
func sealOneOfEveryRecordKind(t *testing.T) []sealedKind {
	t.Helper()
	alice := openCrossProcessDevice(t, filepath.Join(t.TempDir(), "alice"))
	defer alice.close()
	bob := openCrossProcessDevice(t, filepath.Join(t.TempDir(), "bob"))
	defer bob.close()

	groupId := make([]byte, GroupIdBytes)
	if _, err := rand.Read(groupId); err != nil {
		t.Fatalf("drawing a group id: %v", err)
	}
	handle := alice.createGroup(t, groupId)
	defer handle.Close()

	mlsSecret, err := handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	pqSecret, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("pq_secret: %v", err)
	}
	groupHandleKey := messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, pqSecret))

	// THE FOUNDING SESSION IS BUILT BEFORE THE COMMIT MOVES THE HANDLE, which is
	// [Device.CreateGroup]'s own order and is load-bearing: a session installs its epoch's whole
	// key schedule at construction, so this one goes on sealing epoch ZERO records after the
	// handle has moved to epoch one. That is what §4.3.2's self-certified founding commit needs.
	founding := newCrossProcessSession(t, handle, pqSecret, nil, alice.reserver, "a founding nonce")
	defer founding.Close()

	keyPackage, err := bob.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("bob's key package: %v", err)
	}
	if _, err := handle.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	commit, _, _, err := handle.Commit(nil)
	if err != nil {
		handle.ClearPendingCommit()
		t.Fatalf("Commit: %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	epoch := handle.Epoch()
	session := newCrossProcessSession(t, handle, pqSecret, groupHandleKey, alice.reserver, "an epoch one nonce")
	defer session.Close()

	keys, err := session.EpochKeys()
	if err != nil {
		t.Fatalf("this epoch's keys: %v", err)
	}
	defer keys.Destroy()
	writeKey, err := keys.WriteKey()
	if err != nil {
		t.Fatalf("write_key: %v", err)
	}
	readKey, err := keys.ReadKey()
	if err != nil {
		t.Fatalf("read_key: %v", err)
	}
	groupContext, err := handle.GroupContextBytes()
	if err != nil {
		t.Fatalf("the group context: %v", err)
	}
	contextHash := sha256.Sum256(groupContext)
	wrapTarget := messagegroup.WrapTargetHandle(groupHandleKey, epoch, 0)

	nowMs := time.Now().UnixMilli()
	sealed := []sealedKind{}

	// (1) the founding commit, at epoch zero, PERMANENT, is_commit.
	commitRecord, err := founding.SealRecord(message.RetentionPermanent, 0, true,
		encodeHead(nowMs), commit, 0, &message.ServerAttachment{
			Kind: message.AttachmentEpoch,
			Epoch: &message.EpochAttachment{
				Epoch:             epoch,
				AlgId:             epochAttachmentAlgId,
				WriteKey:          writeKey,
				ReadKey:           readKey,
				GroupContextHash:  contextHash[:],
				ExpectedWrapCount: 2,
			},
		})
	if err != nil {
		t.Fatalf("sealing the founding commit: %v", err)
	}
	sealed = append(sealed, sealedKind{"the founding commit", commitRecord})

	// (2) one device wrap, PERMANENT, carrying no key material.
	wrap, err := session.SealRecord(message.RetentionPermanent, 0, false,
		encodeHead(nowMs), []byte(alphaWrapBody), 0, &message.ServerAttachment{
			Kind: message.AttachmentWrap,
			Wrap: &message.WrapTag{WrapTargetHandle: append([]byte(nil), wrapTarget[:]...), Epoch: epoch},
		})
	if err != nil {
		t.Fatalf("sealing an epoch wrap: %v", err)
	}
	sealed = append(sealed, sealedKind{"an epoch wrap", wrap})

	// (3) the marker that closes the fan-out, DURABLE.
	marker, err := session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(nowMs), []byte(alphaEpochCompleteBody), 0, &message.ServerAttachment{
			Kind:     message.AttachmentComplete,
			Complete: &message.EpochComplete{Epoch: epoch, WrapCount: 2},
		})
	if err != nil {
		t.Fatalf("sealing the epoch complete marker: %v", err)
	}
	sealed = append(sealed, sealedKind{"the epoch complete marker", marker})

	// (4) an application record: DURABLE, no attachment, and a CONTENT ENVELOPE as its body,
	// which is the one of the four the 2026-09-17 ruling changed.
	plaintext, err := encodeText("a line, inside a content envelope")
	if err != nil {
		t.Fatalf("encodeText: %v", err)
	}
	application, err := session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(nowMs), plaintext, 0, nil)
	if err != nil {
		t.Fatalf("sealing an application record: %v", err)
	}
	sealed = append(sealed, sealedKind{"an application record", application})

	return sealed
}
