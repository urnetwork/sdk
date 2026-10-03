package urmessage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/sdk"
)

// THE RESTORE CROSSES A REAL PROCESS BOUNDARY, AND UNTIL THIS FILE EXISTED NOTHING IN THIS MODULE
// DID.
//
// WHY IT IS HERE AT ALL. Two places in committed source claimed a process boundary that did not
// exist -- `cp3b/restart_test.go` said its stream-index assertion was "the only assertion in the
// suite that measures it across a process boundary" and `doc.go` said cp3b's restart case
// "measures both across a process boundary at once". Both were false. `cp3b`'s restart closes and
// reopens inside ONE test process, and there was no `os/exec` anywhere in `urmessage` or `cp3b`.
// A published claim that is false is worse than an absent one, so the claim has been corrected in
// both places AND the property it claimed has been built here.
//
// ONE CORRECTION TO THE FINDING ITSELF, because it was stated one directory too wide: `sdk` DOES
// already re-execute its own test binary -- message_stream_store_test.go's
// TestStreamExclusionHelperProcess, which is how the stream store's single-writer exclusion is
// held across a real process. So the precedent for this shape was in the module the whole time;
// what was absent was any cross-process case over the RESTORE. The query:
//
//	grep -rn "os/exec\|exec.Command" --include=*.go urmessage/ cp3b/   -> nothing
//	grep -rn "exec.Command(os.Args\[0\]" --include=*.go .              -> message_stream_store_test.go
//
// WHY A PROCESS BOUNDARY IS NOT THE SAME AS CLOSING EVERYTHING. An in-process restart is a strong
// test and it is not this one: package-level state, a sync.Once, a cached crypto provider, a
// finalizer that has not run, an allocator that still holds the old buffer -- none of those are
// dropped by closing a store, and all of them are dropped by the process dying. What crosses here
// is two directories and one file of opaque ciphertext, and nothing else can.
//
// WHY THERE IS NO SERVER IN IT, AND THAT IS A DECISION RATHER THAN A SHORTCUT. The server holds no
// key material at all: the property "the MLS state and the identity survived the death of the
// process that made them" is entirely between the disk and the key schedule. And a full-seam
// cross-process case is not merely expensive but unreachable -- `DeviceConfig.Transport` is the
// concrete `*sdk.MessageTransport`, `connect` has no inbound listener for client frames, and the
// in-process server `cp3b` stands up cannot be reached from a second process. So the two cases
// measure two different things on purpose: `cp3b` has the whole seam and an in-process restart,
// this has a real process death and no server, and neither replaces the other.
//
// WHAT THE CHILD RUNS IS THIS BINARY. It re-executes os.Args[0] with `-test.run` anchored to this
// test, so the child is the same code with the same build flags -- including `-race` when the
// parent was built with it -- rather than a second program that might differ. That is
// msgrepo/cmd/message-server/restart_test.go's shape and it is taken deliberately.
//
// FOUR ASSERTIONS IN A PROCESS THAT DID NOT EXIST WHEN THE RECORDS WERE SEALED:
//
//  1. THE EXPORTER. The restored MLS group answers the SAME 32 octets under this package's storage
//     label. Every key below it -- storage_root, group_handle_key, the class keys, every record
//     key -- is a function of that value, so this one comparison is the whole key schedule.
//  2. THE IDENTITY. The restored sender_handle is the one the dead process sealed under. A device
//     that came back under a fresh signature key is refused by mls.LoadGroup outright; a device
//     that came back at a different leaf would compute a different handle and collide with
//     nothing.
//  3. READING THE PAST. A record the OTHER party sealed before the death opens, under a receiver
//     ladder followed from its root by a schedule rebuilt off the restored state.
//  4. THE STREAM INDEX DOES NOT REWIND. The next seal takes the index after the dead process's
//     last one, off the durable reserver, UNDER A DIFFERENT SERVER NONCE -- which is what says the
//     nonce is not on the restore path.
func TestTheRestoreCrossesARealProcessBoundary(t *testing.T) {
	if phase := os.Getenv(crossProcessPhaseVariable); phase != "" {
		// this process IS one of the two halves; the parent below is what put it here.
		runCrossProcessPhase(t, phase)
		return
	}
	root := t.TempDir()
	runCrossProcessChild(t, root, crossProcessPhaseSeal)

	// READ BY A THIRD PARTY THAT IS NEITHER CHILD. If the assertions in phase B were to fail,
	// they would fail over a disk this line has just seen -- so "the restore worked" cannot be
	// confused with "the first process wrote nothing and the second found nothing".
	facts := readCrossProcessFacts(t, root)
	if facts.Exporter == "" || facts.SenderHandle == "" || facts.StreamIndex != 1 {
		t.Fatalf("the first process left %+v, which is not a device that sealed anything", facts)
	}
	identity := filepath.Join(root, "bob", "state", stateDataDirName, "device")
	if _, err := os.Stat(identity); err != nil {
		t.Fatalf("the first process left no device identity on the disk, so there is nothing for a second process to come back as: %v", err)
	}

	runCrossProcessChild(t, root, crossProcessPhaseRestore)
}

// The two halves, and the environment that selects one.
//
// A phase rather than a second Test function, because a second Test function would have to skip
// itself in an ordinary run -- and a suite that reports a skip nobody reads is how a test that
// never executes looks exactly like a test that passed.
const (
	crossProcessPhaseVariable = "URMESSAGE_XPROC_PHASE"
	crossProcessRootVariable  = "URMESSAGE_XPROC_ROOT"

	crossProcessPhaseSeal    = "seal"
	crossProcessPhaseRestore = "restore"
)

// The line whose journey spans the process death. Its plaintext is asserted on the far side, so
// that "the record opened" cannot pass over an empty body.
const crossProcessLine = "sealed by alice in the process that died, opened by bob in a process that did not exist yet"

// crossProcessFacts is everything the first process hands the second that is NOT on the two
// directories: the far side's record, and the values phase B holds its own answers against.
//
// NOTHING SECRET CROSSES IN IT. The record is ciphertext, and the other three are a public
// identifier and two values phase B derives for itself and compares -- they are here so that a
// phase B which derived something DIFFERENT is a failure rather than a green test over its own
// arithmetic.
type crossProcessFacts struct {
	Record       string `json:"record"`
	Exporter     string `json:"exporter"`
	SenderHandle string `json:"sender_handle"`
	StreamIndex  uint64 `json:"stream_index"`
	GroupId      string `json:"group_id"`
}

func crossProcessFactsPath(root string) string {
	return filepath.Join(root, "facts.json")
}

func readCrossProcessFacts(t *testing.T, root string) crossProcessFacts {
	t.Helper()
	content, err := os.ReadFile(crossProcessFactsPath(root))
	if err != nil {
		t.Fatalf("the first process left no facts file: %v", err)
	}
	facts := crossProcessFacts{}
	if err := json.Unmarshal(content, &facts); err != nil {
		t.Fatalf("the facts file does not parse: %v", err)
	}
	return facts
}

// runCrossProcessChild starts one half in its own process and fails this test with its output.
func runCrossProcessChild(t *testing.T, root string, phase string) {
	t.Helper()
	child := exec.Command(os.Args[0],
		"-test.run=^TestTheRestoreCrossesARealProcessBoundary$",
		"-test.v",
		"-test.timeout=4m")
	child.Env = append(os.Environ(),
		crossProcessPhaseVariable+"="+phase,
		crossProcessRootVariable+"="+root)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s process failed (%v). The whole point of this case is that the two halves are two processes, so a failure here is a failure of the property and not of the harness.\n%s",
			phase, err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "PHASE") {
			t.Logf("---- %s ---- %s", phase, strings.TrimSpace(line))
		}
	}
}

// runCrossProcessPhase is the child: one half, in its own process, over the directories the parent
// named.
func runCrossProcessPhase(t *testing.T, phase string) {
	root := os.Getenv(crossProcessRootVariable)
	if root == "" {
		t.Fatalf("%s is set and %s is not, so this child has no directory to be half of a restart against",
			crossProcessPhaseVariable, crossProcessRootVariable)
	}
	switch phase {
	case crossProcessPhaseSeal:
		crossProcessSeal(t, root)
	case crossProcessPhaseRestore:
		crossProcessRestore(t, root)
	default:
		t.Fatalf("%s=%q is not a phase of this test", crossProcessPhaseVariable, phase)
	}
}

// ── phase A: a group is founded, joined and sealed into, and then this process is gone ───────

func crossProcessSeal(t *testing.T, root string) {
	alice := openCrossProcessDevice(t, filepath.Join(root, "alice"))
	bob := openCrossProcessDevice(t, filepath.Join(root, "bob"))

	groupId := make([]byte, GroupIdBytes)
	if _, err := rand.Read(groupId); err != nil {
		t.Fatalf("drawing a group id: %v", err)
	}
	aliceHandle := alice.createGroup(t, groupId)

	// AT EPOCH ZERO AND NOWHERE ELSE, which is device.go's rule and is restated here because a
	// group_handle_key recomputed from a later root gives every epoch a different sender_handle.
	mlsSecret, err := aliceHandle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	pqSecret, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("pq_secret: %v", err)
	}
	groupHandleKey := messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, pqSecret))

	// the commit that opens epoch one, which is the alpha's only epoch.
	keyPackage, err := bob.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("bob's key package: %v", err)
	}
	// ProposeAdd then Commit(nil), which is [Group.AddMember]'s own pair: the proposal is
	// staged in the handle and the commit takes what is pending rather than a reference this
	// test would have to cache.
	if _, err := aliceHandle.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	_, welcome, ratchetTree, err := aliceHandle.Commit(nil)
	if err != nil {
		aliceHandle.ClearPendingCommit()
		t.Fatalf("Commit: %v", err)
	}
	if err := aliceHandle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	bobHandle, err := bob.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("bob's JoinFromWelcome: %v", err)
	}

	// TWO DIFFERENT SERVER NONCES, one per side, because this phase is also what makes phase B's
	// nonce assertion mean something: the nonce is a per-connection value and it must not be on
	// the restore path at all.
	aliceSession := newCrossProcessSession(t, aliceHandle, pqSecret, groupHandleKey, alice.reserver, "alice's connection nonce")
	bobSession := newCrossProcessSession(t, bobHandle, pqSecret, groupHandleKey, bob.reserver, "bob's connection nonce")

	// bob seals first, so the restore has a stream index to continue FROM rather than a fresh
	// row to allocate the first index of.
	bobRecord, err := bobSession.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), []byte("bob speaks before his process dies"), 0, nil)
	if err != nil {
		t.Fatalf("bob's SealRecord: %v", err)
	}
	if bobRecord.Header.StreamIndex != 1 {
		t.Fatalf("bob's first seal took stream index %d, want 1", bobRecord.Header.StreamIndex)
	}
	aliceRecord, err := aliceSession.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), []byte(crossProcessLine), 0, nil)
	if err != nil {
		t.Fatalf("alice's SealRecord: %v", err)
	}
	encoded, err := message.EncodeRecord(aliceRecord)
	if err != nil {
		t.Fatalf("encoding alice's record: %v", err)
	}

	// bob's urmessage-side record, which is what Device.Restore reads back.
	if err := bob.store.PutGroupRecord(&GroupRecord{
		GroupId:        groupId,
		PqSecret:       pqSecret,
		GroupHandleKey: groupHandleKey,
		Epoch:          bobHandle.Epoch(),
		Opened:         true,
	}); err != nil {
		t.Fatalf("bob's group record: %v", err)
	}

	bobExporter, err := bobHandle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("bob's exporter: %v", err)
	}
	bobOwn, err := bobSession.SenderHandle()
	if err != nil {
		t.Fatalf("bob's sender handle: %v", err)
	}
	facts := crossProcessFacts{
		Record:       hex.EncodeToString(encoded),
		Exporter:     hex.EncodeToString(bobExporter),
		SenderHandle: hex.EncodeToString(bobOwn[:]),
		StreamIndex:  bobRecord.Header.StreamIndex,
		GroupId:      hex.EncodeToString(groupId),
	}
	content, err := json.Marshal(&facts)
	if err != nil {
		t.Fatalf("marshalling the facts: %v", err)
	}
	if err := os.WriteFile(crossProcessFactsPath(root), content, 0o600); err != nil {
		t.Fatalf("writing the facts: %v", err)
	}
	t.Logf("PHASE A: group %s at epoch %d, bob at stream index %d, exporter %s, alice's record %d octets",
		facts.GroupId[:16], bobHandle.Epoch(), facts.StreamIndex, facts.Exporter[:16], len(encoded))

	// and everything is CLOSED before this process exits, so phase B has to acquire both
	// exclusions itself. A phase B that opened over a directory this process still held would
	// be refused, which is the second thing that says the process really went away.
	aliceSession.Close()
	bobSession.Close()
	aliceHandle.Close()
	bobHandle.Close()
	alice.close()
	bob.close()
}

// ── phase B: a process that did not exist when any of that happened ──────────────────────────

func crossProcessRestore(t *testing.T, root string) {
	facts := readCrossProcessFacts(t, root)

	// THE THREE CALLS BELOW ARE Device.restoreOne'S BODY, in its order, and that is deliberate:
	// a case that rebuilt the group some other way would be measuring its own arithmetic rather
	// than the restore path this package ships.
	//
	// IT WAS FOUR UNTIL J1-8 CLOSED, and the pair that collapsed is the finding rather than a
	// tidy-up. restoreOne used to call `mls.LoadGroup` itself and wrap the result in a handle this
	// package declared, so this case did the same two things. `messagegroup.GroupEngine` grew
	// LoadGroup, both went away in the same commit, and this case now drives the engine door --
	// which is what keeps its own header sentence true rather than one commit stale.
	bob := openCrossProcessDevice(t, filepath.Join(root, "bob"))
	defer bob.close()

	records, err := bob.store.GroupRecords()
	if err != nil {
		t.Fatalf("PHASE B: GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("PHASE B: the disk holds %d group record(s) and the dead process wrote one", len(records))
	}
	record := records[0]
	if hex.EncodeToString(record.GroupId) != facts.GroupId {
		t.Fatalf("PHASE B: the disk holds group %x and the dead process founded %s",
			record.GroupId, facts.GroupId)
	}
	handle, err := bob.engine.LoadGroup(record.GroupId, record.Epoch)
	if err != nil {
		t.Fatalf("PHASE B: GroupEngine.LoadGroup at epoch %d: %v", record.Epoch, err)
	}
	defer handle.Close()

	// (1) THE EXPORTER, in a process that did not derive it.
	exporter, err := handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("PHASE B: the restored exporter: %v", err)
	}
	if hex.EncodeToString(exporter) != facts.Exporter {
		t.Fatalf("PHASE B: the restored mls exporter answers %s and the dead process had %s; every key below it is a different key",
			hex.EncodeToString(exporter), facts.Exporter)
	}
	t.Logf("PHASE B: the mls exporter answers the same %d octets in a NEW PROCESS: %s",
		len(exporter), hex.EncodeToString(exporter)[:16])

	// A DIFFERENT SERVER NONCE, which is the point: a nonce is per connection, the dead process
	// had another one, and if the restore depended on it nothing below would open.
	session := newCrossProcessSession(t, handle, record.PqSecret, record.GroupHandleKey,
		bob.reserver, "a nonce this connection issued, which no earlier process ever saw")
	defer session.Close()

	// (2) THE IDENTITY.
	own, err := session.SenderHandle()
	if err != nil {
		t.Fatalf("PHASE B: the restored sender handle: %v", err)
	}
	if hex.EncodeToString(own[:]) != facts.SenderHandle {
		t.Fatalf("PHASE B: the restored device seals under sender_handle %x and the dead process sealed under %s, so it is a different leaf",
			own, facts.SenderHandle)
	}

	// (3) READING THE PAST: a record the far side sealed before the death.
	raw, err := hex.DecodeString(facts.Record)
	if err != nil {
		t.Fatalf("PHASE B: the carried record does not decode: %v", err)
	}
	parsed, err := message.ParseRecord(raw)
	if err != nil {
		t.Fatalf("PHASE B: the carried record does not parse: %v", err)
	}
	leaf, known := crossProcessLeafOf(t, handle, record.GroupHandleKey, parsed.Header.SenderHandle)
	if !known {
		t.Fatalf("PHASE B: the record names sender_handle %x, which is no leaf of the restored group", parsed.Header.SenderHandle)
	}
	if err := session.TrackSender(leaf, parsed.Header.RetentionClass, parsed.Header.EphBucket,
		parsed.Header.EphWindow, 0); err != nil {
		t.Fatalf("PHASE B: TrackSender(%d): %v", leaf, err)
	}
	_, bodyPlain, err := session.OpenRecord(parsed)
	if err != nil {
		t.Fatalf("PHASE B: the record sealed before this process existed did not open: %v", err)
	}
	if string(bodyPlain) != crossProcessLine {
		t.Fatalf("PHASE B: the record opened to %q", bodyPlain)
	}
	t.Logf("PHASE B: opened a pre-death record in a new process: %q", bodyPlain)

	// (4) THE STREAM INDEX DOES NOT REWIND, off the DURABLE reserver and under the new nonce.
	next, err := session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), []byte("and the restored device seals again"), 0, nil)
	if err != nil {
		t.Fatalf("PHASE B: the restored device could not seal: %v", err)
	}
	if next.Header.StreamIndex <= facts.StreamIndex {
		t.Fatalf("PHASE B: the restored device sealed at stream index %d and the dead process had already used %d; section 5.6 calls a reused index a total break of both AEADs for that record",
			next.Header.StreamIndex, facts.StreamIndex)
	}
	t.Logf("PHASE B: bob's stream index went %d -> %d across a process death",
		facts.StreamIndex, next.Header.StreamIndex)
}

// crossProcessLeafOf is [Group.leavesLocked]'s derivation, one value at a time: which leaf a
// sender_handle belongs to, DERIVED and never read off the record.
func crossProcessLeafOf(t *testing.T, handle messagegroup.GroupHandle, groupHandleKey []byte,
	senderHandle [16]byte) (uint32, bool) {

	t.Helper()
	for at := 0; at < handle.MemberCount(); at += 1 {
		leaf, _, _, err := handle.MemberAt(at)
		if err != nil {
			t.Fatalf("member %d: %v", at, err)
		}
		if messagegroup.SenderHandle(groupHandleKey, leaf) == senderHandle {
			return leaf, true
		}
	}
	return 0, false
}

// ── one device, opened the way NewDevice opens one ───────────────────────────────────────────

// crossProcessDevice is what [NewDevice] builds, minus the transport this property does not need.
// It is spelled out rather than wrapped so that the identity path under test -- deviceIdentity
// over a DurableStateStore -- is the package's own function and not a second copy of it.
type crossProcessDevice struct {
	store       *DurableStateStore
	streamStore *sdk.StreamStore
	reserver    messagegroup.StreamIndexReserver
	crypto      mls.CryptoProvider
	engine      messagegroup.GroupEngine
	signer      mls.SignaturePrivateKey
	identityPub mls.SignaturePublicKey
	leafKeys    []byte

	// the fourth value deviceIdentity answers: the X-Wing seed under the public half inside
	// leafKeys. Held here for the same reason the signer is -- so a phase-B process can be
	// asked whether the disk gave it back.
	wrapSeed []byte
}

func openCrossProcessDevice(t *testing.T, root string) *crossProcessDevice {
	t.Helper()
	store, err := OpenDurableStateStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("OpenDurableStateStore(%s): %v", root, err)
	}
	streamStore, err := sdk.OpenStreamStore(filepath.Join(root, "stream"))
	if err != nil {
		store.Close()
		t.Fatalf("sdk.OpenStreamStore(%s): %v", root, err)
	}
	crypto, err := mls.NewCryptoProvider(deviceCipherSuite)
	if err != nil {
		t.Fatalf("the mls crypto provider: %v", err)
	}
	// THE IDENTITY PATH UNDER TEST. In phase A this mints and writes; in phase B, in a process
	// that never saw the first, it reads back.
	signer, signerPub, leafKeys, wrapSeed, err := deviceIdentity(crypto, store, rand.Reader)
	if err != nil {
		t.Fatalf("deviceIdentity: %v", err)
	}
	engine, err := messagegroup.NewConnectMlsEngine(crypto, store, signer,
		mls.BasicCredential(signerPub), leafKeys)
	if err != nil {
		t.Fatalf("the mls engine: %v", err)
	}
	return &crossProcessDevice{
		store:       store,
		streamStore: streamStore,
		reserver:    sdk.NewStreamIndexReserver(streamStore),
		crypto:      crypto,
		engine:      engine,
		signer:      append(mls.SignaturePrivateKey(nil), signer...),
		identityPub: append(mls.SignaturePublicKey(nil), signerPub...),
		leafKeys:    leafKeys,
		wrapSeed:    wrapSeed,
	}
}

func (self *crossProcessDevice) close() {
	self.store.Close()
	self.streamStore.Close()
}

// createGroup is [Device.createMlsGroup]'s body: the policy names this device as owner, it is
// canonicalized before it is encoded because the extension travels inside the group context, and
// two members that encoded it differently would export different secrets.
func (self *crossProcessDevice) createGroup(t *testing.T, groupId []byte) messagegroup.GroupHandle {
	t.Helper()
	policy := &mls.GroupPolicyExtension{
		Roles: []mls.RoleEntry{{MemberId: self.identityPub, Role: mls.RoleOwner}},
	}
	if err := policy.Canonicalize(); err != nil {
		t.Fatalf("the group policy: %v", err)
	}
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("the group policy: %v", err)
	}
	handle, err := self.engine.CreateGroup(groupId, encoded.ExtensionData, self.leafKeys)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return handle
}

func newCrossProcessSession(t *testing.T, handle messagegroup.GroupHandle, pqSecret []byte,
	groupHandleKey []byte, reserver messagegroup.StreamIndexReserver, nonce string) *messagegroup.GroupSession {

	t.Helper()
	session, err := messagegroup.NewGroupSession(handle, pqSecret, groupHandleKey, reserver,
		func() int64 { return time.Now().UnixMilli() }, []byte(nonce))
	if err != nil {
		t.Fatalf("the session at epoch %d: %v", handle.Epoch(), err)
	}
	return session
}
