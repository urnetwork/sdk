// Three real URnetwork accounts, three real platform connections, one DEPLOYED message server, and
// the whole of what the alpha can do, scenario by scenario -- including the membership change that
// makes a group chat a group chat.
//
// Everything before this ran two connect.Clients over in-process Routes in one binary. This
// crosses the operator's mesh to a server it did not start.
//
// EVERY STEP PRINTS WHAT IT ASSERTED. A probe that prints "OK" without naming what it checked is
// useless at 2am, so each check() below carries the sentence a reader needs in order to know what
// went wrong, and fail() names the step, the expectation and what actually came back.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/messagegroup"
	"github.com/urnetwork/connect/v2026/mls"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/sdk/v2026/urmessage"
)

type party struct {
	name string
	dir  string

	client      interface{ Close() }
	transport   *sdk.MessageTransport
	streamStore *sdk.StreamStore
	stateStore  *urmessage.DurableStateStore
	device      *urmessage.Device
}

// dialer is everything dial() needs that does not change across a restart. It is a struct so that
// a restart is "the same dialer, the same directory, everything else new" rather than eight
// arguments threaded through by hand.
type dialer struct {
	ctx     context.Context
	server  connect.Id
	host    string
	root    string
	timeout time.Duration

	// which client a party speaks over: see routeClient
	route    string
	endpoint string
	pin      string

	// connect is handed to every device this dialer stands up, so that a re-dial rides out the
	// operator's reconnect window instead of reporting it as a failure.
	connect urmessage.ConnectPolicy
}

var (
	steps   int
	checks  int
	current string
)

// ── the transcript, which this run reads back at the end ─────────────────────────────────────

// transcriptLog is every octet this probe writes, KEPT, so that the run can assert a property
// about its own log: that no credential ever reached it.
//
// WHY A PROBE READS ITS OWN OUTPUT. The three things this binary is given are `by_client_jwt`s --
// bearer credentials for three real network clients on a live operator -- and its audience is an
// operator who redirects the output into a file, pastes it into a ticket and leaves it in a
// terminal scrollback. A probe that printed one would have published it, and no reading of this
// source can promise that no `%v` of an error anywhere below carries one, because the errors come
// from four packages this file does not own. So the promise is MEASURED, over the octets that were
// actually written, rather than argued.
//
// WHAT IT COVERS AND WHAT IT DOES NOT, stated rather than implied. It covers everything written
// through `out` and `errOut`, which is every print this file makes. `errOut` is in it so that a FAIL
// line is scanned too -- and the tee alone does not buy that, it only makes it possible: what buys it
// is [scanForCredentials] being called from inside [fail], because every failure path here ends in
// os.Exit and a scan placed only in the final step would never run over a run that failed. It does NOT cover a
// library writing to this process's stdout by its own hand: catching those would need the file
// descriptor itself replaced by a pipe, which costs a drained goroutine and loses whatever is still
// in flight when [fail] calls os.Exit.
type transcriptLog struct {
	mutex sync.Mutex
	held  []byte
	// announced is whether the disclosure sentence has already been printed. It lives HERE, under
	// the same lock as the octets it is about, rather than beside [scanForCredentials] as a package
	// variable: the scan has two callers and one of them is [fail], so "only the main goroutine can
	// reach it" is an argument about today's call graph and not a property of the type. Under the
	// lock it needs no argument.
	announced bool
}

// tee is a writer that goes to `to` AND into this transcript.
//
// THE LOCK IS NOT DECORATIVE. [urmessage.ConnectPolicy]'s OnAttempt is called from inside
// Device.Connect rather than from this goroutine, so a transcript that assumed one printing
// goroutine would be a race, and the property asserted over it would be asserted over a value the
// race could have truncated.
func (self *transcriptLog) tee(to io.Writer) io.Writer {
	return &transcriptWriter{log: self, to: to}
}

// count is how many times a needle occurs in everything written so far, and whether THIS caller is
// the one that gets to announce it.
//
// THE TWO ANSWERS COME OUT OF ONE CRITICAL SECTION because they are one decision. A count read and an
// announcement claimed separately can interleave into two disclosures for one hit, or -- worse -- a
// claim taken against a count that a concurrent write has since moved.
func (self *transcriptLog) count(needle string) (held int, announce bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	held = strings.Count(string(self.held), needle)
	announce = 0 < held && !self.announced
	if announce {
		self.announced = true
	}
	return held, announce
}

// octets is how much has been written, which is what makes a count of zero non-vacuous.
func (self *transcriptLog) octets() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return len(self.held)
}

type transcriptWriter struct {
	log *transcriptLog
	to  io.Writer
}

func (self *transcriptWriter) Write(p []byte) (int, error) {
	self.log.mutex.Lock()
	self.log.held = append(self.log.held, p...)
	self.log.mutex.Unlock()
	return self.to.Write(p)
}

var (
	transcript = &transcriptLog{}

	// out and errOut are where this probe prints. Nothing below writes to os.Stdout or os.Stderr
	// directly, because a print that bypassed these would be a print the credential scan cannot see.
	out    = transcript.tee(os.Stdout)
	errOut = transcript.tee(os.Stderr)
)

// credentialNeedle is the first three octets of every JWT this system mints: the base64url of the
// beginning of `{"alg`. A by_client_jwt starts with it, and so does any JWT a formatted error could
// carry. It is deliberately NOT written to `out` anywhere -- a success line that printed it would
// put a hit in the very log an operator greps.
const credentialNeedle = "eyJ"

// credentialControl is the CONTROL for the credential assertion, and it is a FABRICATED value that
// has never authenticated anything: an HS256 header, a payload that says what it is, and a
// signature that is not one. It exists so that "the log holds no credential" is a measurement
// rather than a property of a scanner that matches nothing, and it is never written to `out`.
const credentialControl = "eyJhbGciOiJIUzI1NiJ9.THIS-IS-NOT-A-CREDENTIAL.not-a-signature"

func main() {
	aJwt := flag.String("a", "", "file holding party A's by_client_jwt")
	bJwt := flag.String("b", "", "file holding party B's by_client_jwt")
	cJwt := flag.String("c", "", "file holding party C's by_client_jwt; C is the third member step 5 adds, so it is required")
	serverId := flag.String("server", "", "the message server's client_id")
	host := flag.String("host", "beta-test.net", "operator host")
	dir := flag.String("dir", "/var/lib/urmessage/probe", "where each party's durable state lives")
	text := flag.String("text", "the first message over the real mesh", "what A sends first")
	lines := flag.Int("lines", 600, "how many messages step 4 sends; must exceed the server's max_records_per_fetch to reach the truncation path")
	bigBytes := flag.Int("big", 40000, "how many octets step 6's message carries; anything over 2048 crosses the fragmentation cut")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request transport timeout")
	reconnect := flag.Duration("reconnect", 0,
		"how long Device.Connect rides out the ~60s reconnect window; 0 takes urmessage's own default")
	route := flag.String("route", "platform", "platform (the operator path), urnetwork (the server's own endpoint through a URnetwork exit) or direct (that endpoint, no tunnel)")
	endpoint := flag.String("endpoint", "", "the server's own endpoint, a wss:// url; required unless -route platform")
	pin := flag.String("pin", "", "SHA-256 of the endpoint's SubjectPublicKeyInfo, 64 hex characters; required unless -route platform")
	flag.Parse()

	server, err := connect.ParseId(*serverId)
	if err != nil {
		fail("parse server id: %v", err)
	}
	// REFUSED HERE AND NOT IN STEP 5, because step 5 is four minutes and six hundred records in,
	// and a probe that spends them before saying it needed a third credential has wasted them.
	if *cJwt == "" {
		fail("-c is required: step 5 adds a THIRD real device to the group, and a third device is a third credential")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lineCount := *lines
	// THE RECONNECT WINDOW IS THE OPERATOR'S AND THIS PROBE HAS TO SURVIVE IT RATHER THAN TRIP
	// OVER IT. Measured on the deployed server: a client_id that has just re-dialled is NOT
	// ROUTED TO for about sixty seconds -- the connection attaches, the Hello goes out and
	// nothing comes back. Step 7 re-dials B for the same client_id, so step 7 lands in that
	// window every time, and before urmessage.Device.Connect retried, step 7 reported a hard
	// failure for something that was simply not ready. Filed against the operator as item 5 of
	// docs/reports/2026-09-15-operator-and-connect-findings.md; NOT closed by this, and the
	// user still waits the sixty seconds.
	mesh := &dialer{ctx: ctx, server: server, host: *host, root: *dir, timeout: *timeout,
		route: *route, endpoint: *endpoint, pin: *pin,
		connect: urmessage.ConnectPolicy{Budget: *reconnect, OnAttempt: printConnectAttempt}}

	a := mesh.dial("A", *aJwt)
	b := mesh.dial("B", *bJwt)
	defer a.close()
	defer b.close()

	// ── 1 ────────────────────────────────────────────────────────────────────────────────
	step("Hello on both parties, and the capabilities the server advertises")
	for _, p := range []*party{a, b} {
		hctx, hcancel := context.WithTimeout(ctx, *timeout)
		reason, hello, err := p.transport.Hello(hctx, 1)
		hcancel()
		if err != nil {
			fail("%s hello: %v", p.name, err)
		}
		check(reason == protocol.Reason_REASON_OK, "%s: Hello answered %v, want REASON_OK", p.name, reason)
		check(len(hello.GetServerNonce()) != 0,
			"%s: Hello issued no server_nonce, and every authenticator this client computes is a MAC over one", p.name)
		capabilities := hello.GetCapabilities()
		fmt.Fprintf(out, "  %s  nonce %d octets; max_records_per_fetch=%d max_request_bytes=%d attestation_supported=%v\n",
			p.name, len(hello.GetServerNonce()), capabilities.GetMaxRecordsPerFetch(),
			capabilities.GetMaxRequestBytes(), capabilities.GetAttestationSupported())
		if !capabilities.GetAttestationSupported() {
			fmt.Fprintf(out, "  %s  NOTE: this server advertises NO 4.3.4 attestation, so a server that OMITS\n"+
				"        records from a fetch page is undetectable by this client. S2-27, and it is\n"+
				"        counted per page in Stats.Unattested rather than assumed away.\n", p.name)
		}
		if err := p.device.Connect(ctx); err != nil {
			fail("%s Connect: %v", p.name, err)
		}
	}

	// ── 2 ────────────────────────────────────────────────────────────────────────────────
	step("A founds a group, adds B, and publishes it on the server")
	groupId := make([]byte, urmessage.GroupIdBytes)
	if _, err := rand.Read(groupId); err != nil {
		fail("group id: %v", err)
	}
	aGroup, err := a.device.CreateGroup(ctx, groupId)
	if err != nil {
		fail("A CreateGroup: %v", err)
	}
	check(aGroup.Epoch() == 0, "a freshly founded group is at epoch %d, want 0", aGroup.Epoch())
	keyPackage, err := b.device.KeyPackage()
	if err != nil {
		fail("B KeyPackage: %v", err)
	}
	invite, err := aGroup.AddMember(keyPackage)
	if err != nil {
		fail("A AddMember: %v", err)
	}
	check(aGroup.Epoch() == 1, "the commit that added B left the group at epoch %d, want 1", aGroup.Epoch())
	encoded, err := invite.Encode()
	if err != nil {
		fail("encode invite: %v", err)
	}
	if err := aGroup.Open(ctx); err != nil {
		fail("A Open: %v", err)
	}
	check(aGroup.IsOpen(), "Open returned no error and the group does not say it is open")
	fmt.Fprintf(out, "  group %x epoch %d; invite %d octets, carried out of band as the design says\n",
		aGroup.Id()[:8], aGroup.Epoch(), len(encoded))

	// ── 3 ────────────────────────────────────────────────────────────────────────────────
	step("B joins from the invite and reads the string A typed")
	carried, err := urmessage.ParseInvite(encoded)
	if err != nil {
		fail("parse invite: %v", err)
	}
	sent, err := aGroup.Send(ctx, *text)
	if err != nil {
		fail("A Send: %v", err)
	}
	bGroup, err := b.device.Join(ctx, carried)
	if err != nil {
		fail("B Join: %v", err)
	}
	got := receive(bGroup, ctx, "B")
	check(len(got) == 1, "B read %d messages for the one A sent: %s", len(got), texts(got))
	check(got[0].Text == *text, "B read %q and A typed %q", got[0].Text, *text)
	check(got[0].RecordId == sent.RecordId, "B opened record %d and A was told hers is record %d",
		got[0].RecordId, sent.RecordId)
	check(!got[0].Mine, "a message B received from A came back marked as B's own")
	reply := "and the answer came back"
	if _, err := bGroup.Send(ctx, reply); err != nil {
		fail("B Send: %v", err)
	}
	back := receive(aGroup, ctx, "A")
	check(len(back) == 1 && back[0].Text == reply, "A read %s for B's one answer", texts(back))
	fmt.Fprintf(out, "  the string crossed both ways; B's sender_handle %x\n", back[0].SenderHandle)

	// ── 4 ────────────────────────────────────────────────────────────────────────────────
	step(fmt.Sprintf("%d messages in order, which is what crosses a fetch page", lineCount))
	fmt.Fprintf(out, "  WHAT THIS IS FOR: 4.3.4 truncates a page by `limit` OR by `max_response_bytes` and\n"+
		"  calls both NORMAL. Receive must page until the server says complete. A build that read\n"+
		"  one page returns the first screenful of a conversation with a NIL ERROR, which a user\n"+
		"  cannot tell from a quiet room.\n")
	typed := make([]string, 0, lineCount)
	for at := 0; at < lineCount; at += 1 {
		line := fmt.Sprintf("line %d of %d over the real mesh", at+1, lineCount)
		if _, err := aGroup.Send(ctx, line); err != nil {
			fail("A Send line %d: %v", at+1, err)
		}
		typed = append(typed, line)
	}
	pagesBefore := bGroup.Stats().Pages
	bulk := receive(bGroup, ctx, "B")
	pages := bGroup.Stats().Pages - pagesBefore
	check(len(bulk) == len(typed), "A sent %d lines and ONE Receive answered %d", len(typed), len(bulk))
	for at := range typed {
		check(bulk[at].Text == typed[at], "message %d came back as %q and was typed as %q",
			at, bulk[at].Text, typed[at])
		if 0 < at {
			check(bulk[at-1].RecordId < bulk[at].RecordId,
				"message %d is record %d and message %d is record %d, so the order is not the server's",
				at, bulk[at].RecordId, at-1, bulk[at-1].RecordId)
		}
	}
	fmt.Fprintf(out, "  %d lines came back in record order, in ONE Receive, over %d fetch page(s)\n", len(bulk), pages)
	if pages < 2 {
		fmt.Fprintf(out, "  WARNING: one page carried all of it, so the truncation path was NOT exercised.\n"+
			"  Raise -lines above this server's max_records_per_fetch (printed in step 1) and run again.\n")
	}

	// ── 5 ────────────────────────────────────────────────────────────────────────────────
	step("a THIRD member is added to a group that has been chatting: a second epoch, on the real mesh")
	fmt.Fprintf(out, "  WHAT THIS IS FOR: this is where group chats start to exist. Everything above ran at\n"+
		"  epoch 1, the one epoch the founding add opened. A adds C to the OPEN group, which is a\n"+
		"  commit sealed at epoch 1 announcing epoch 2, a wrap fan-out for the new epoch and a marker\n"+
		"  -- all submitted to the deployed server. C joins from a Welcome. B, who authored nothing,\n"+
		"  learns of it only by fetching the commit and INGESTING it, and must follow into epoch 2.\n"+
		"  Then one line from each device opens on both others, which is the assertion that says the\n"+
		"  three share one epoch-2 key schedule rather than merely agreeing on the number 2. The\n"+
		"  founding-time AddMember still refuses a second add by name; this is the other door.\n")
	// THE EPOCH-ONE LINES ARE COUNTED HERE, once, because two later assertions are about what became
	// of them: A's first text, B's answer, and step 4's lines. Nothing else at epoch 1 is a line.
	epochOneLines := 2 + lineCount
	c := mesh.dial("C", *cJwt)
	defer c.close()
	if err := c.device.Connect(ctx); err != nil {
		fail("C Connect: %v", err)
	}
	thirdKeyPackage, err := c.device.KeyPackage()
	if err != nil {
		fail("C KeyPackage: %v", err)
	}
	epochBefore := aGroup.Epoch()
	thirdInvite, err := aGroup.AddMemberAndPublish(ctx, thirdKeyPackage)
	if err != nil {
		fail("A AddMemberAndPublish: %v", err)
	}
	check(aGroup.Epoch() == epochBefore+1, "the commit that added C left A at epoch %d, want %d", aGroup.Epoch(), epochBefore+1)
	check(aGroup.Epoch() == 2, "A is at epoch %d after the second add, want 2", aGroup.Epoch())
	thirdEncoded, err := thirdInvite.Encode()
	if err != nil {
		fail("encode C's invite: %v", err)
	}
	thirdCarried, err := urmessage.ParseInvite(thirdEncoded)
	if err != nil {
		fail("parse C's invite: %v", err)
	}
	cGroup, err := c.device.Join(ctx, thirdCarried)
	if err != nil {
		fail("C Join: %v", err)
	}
	check(cGroup.Epoch() == 2, "C joined at epoch %d, want 2", cGroup.Epoch())
	check(string(cGroup.Id()) == string(groupId), "C joined group %x and A founded %x", cGroup.Id()[:8], groupId[:8])
	fmt.Fprintf(out, "  A's commit opened epoch %d on the server; C joined from a %d octet invite at epoch %d\n",
		aGroup.Epoch(), len(thirdEncoded), cGroup.Epoch())

	// B INGESTS THE COMMIT. This is the one arm neither the committer (who merges its own commit)
	// nor the joiner (who is handed a Welcome) exercises, and it is asserted on B's own counters:
	// Ingested moves once, the epoch follows, and NOTHING of B's history goes out of reach, because
	// B was current when the epoch moved. A commit is not a line, so the Receive hands back none.
	ingestedBefore := bGroup.Stats().Ingested
	atIngest := receive(bGroup, ctx, "B")
	check(bGroup.Epoch() == 2, "B did not follow A's commit into epoch 2: B is at epoch %d", bGroup.Epoch())
	check(bGroup.Stats().Ingested == ingestedBefore+1, "B's Stats.Ingested is %d after following one commit, want %d",
		bGroup.Stats().Ingested, ingestedBefore+1)
	check(bGroup.Stats().Ingested == 1, "B has ingested %d commit(s) in its life and there has been exactly one", bGroup.Stats().Ingested)
	check(len(atIngest) == 0, "B's ingesting Receive handed back %d entries and a commit is not a line: %s", len(atIngest), texts(atIngest))
	check(bGroup.Stats().GapOutOfWindow == 0,
		"B was current when the epoch moved and still has %d out_of_window gap(s); an up-to-date member must lose nothing to a commit",
		bGroup.Stats().GapOutOfWindow)
	fmt.Fprintf(out, "  B ingested the commit on its next Receive and followed into epoch %d (Stats.Ingested=%d), with 0 gaps\n",
		bGroup.Epoch(), bGroup.Stats().Ingested)

	// C DRAINS THE PRE-JOIN HISTORY. C holds no epoch-1 key schedule, so every epoch-1 line is a
	// record it cannot open -- an out_of_window GAP, one per line, and NOT a failure. Counted
	// exactly rather than tolerated: it is the number item 241's history-for-new-members owes.
	drained := receive(cGroup, ctx, "C")
	cGaps, cOpened, cOtherGaps := gapCount(drained)
	check(cOpened == 0, "C opened %d line(s) from before it was a member; it holds no key that could", cOpened)
	check(cOtherGaps == 0, "C's drain produced %d gap(s) of a reason other than out_of_window", cOtherGaps)
	check(cGaps == epochOneLines, "C's drain produced %d out_of_window gap(s) and the group exchanged %d line(s) at epoch 1",
		cGaps, epochOneLines)
	check(cGroup.Stats().GapOutOfWindow == uint64(cGaps), "C's Stats.GapOutOfWindow is %d and the drain handed back %d gaps",
		cGroup.Stats().GapOutOfWindow, cGaps)
	fmt.Fprintf(out, "  C drained %d pre-join record(s) as out_of_window gaps, opened 0, failed 0 -- the history it was not there for\n", cGaps)

	// ALL THREE AT EPOCH TWO.
	check(aGroup.Epoch() == 2 && bGroup.Epoch() == 2 && cGroup.Epoch() == 2,
		"epochs did not converge: A %d, B %d, C %d", aGroup.Epoch(), bGroup.Epoch(), cGroup.Epoch())

	// SIX DIRECTIONS. One line from each device, opened on both others, every one asserted on the
	// FAR side against the exact text and against being a line rather than a gap.
	const (
		aAtTwo = "epoch two: A, to a group that now has three members"
		bAtTwo = "epoch two: B, who followed a commit it did not author"
		cAtTwo = "epoch two: C, the third member, sealing under its own new leaf"
	)
	if _, err := aGroup.Send(ctx, aAtTwo); err != nil {
		fail("A Send at epoch 2: %v", err)
	}
	opens(bGroup, ctx, "B", "A", aAtTwo)
	opens(cGroup, ctx, "C", "A", aAtTwo)
	if _, err := bGroup.Send(ctx, bAtTwo); err != nil {
		fail("B Send at epoch 2: %v", err)
	}
	opens(aGroup, ctx, "A", "B", bAtTwo)
	opens(cGroup, ctx, "C", "B", bAtTwo)
	if _, err := cGroup.Send(ctx, cAtTwo); err != nil {
		fail("C Send at epoch 2: %v", err)
	}
	opens(aGroup, ctx, "A", "C", cAtTwo)
	opens(bGroup, ctx, "B", "C", cAtTwo)
	for _, member := range []struct {
		name  string
		group *urmessage.Group
	}{{"A", aGroup}, {"B", bGroup}, {"C", cGroup}} {
		check(member.group.Stats().FailedOpen == 0, "%s failed to open %d record(s) across the epoch change",
			member.name, member.group.Stats().FailedOpen)
	}
	fmt.Fprintf(out, "  six directions at epoch 2: A->B A->C B->A B->C C->A C->B, each opened on the far side with the exact text\n")

	// ── 6 ────────────────────────────────────────────────────────────────────────────────
	step(fmt.Sprintf("a %d octet message, which no live test has ever fragmented", *bigBytes))
	fmt.Fprintf(out, "  WHAT THIS IS FOR: 4.6 cuts a request into 2048 octet parts, so this one crosses as\n"+
		"  roughly %d frames and is reassembled on the far side. Everything before it fitted one\n"+
		"  frame. If the text comes back with a single octet changed, the reassembler is joining\n"+
		"  parts in the wrong order and no AEAD downstream would ever have told you which.\n",
		(*bigBytes/2048)+1)
	big := strings.Repeat("0123456789abcdef", (*bigBytes/16)+1)[:*bigBytes]
	bigSent, err := aGroup.Send(ctx, big)
	if err != nil {
		fail("A Send of %d octets: %v -- if this is ErrTextTooLong the text is above the largest inline size bucket and -big must come down",
			*bigBytes, err)
	}
	bigGot := receive(bGroup, ctx, "B")
	check(len(bigGot) == 1, "B read %d messages for the one large one A sent", len(bigGot))
	check(bigGot[0].Text == big, "the %d octet text came back changed: %d octets, first difference at %d",
		len(big), len(bigGot[0].Text), firstDifference(big, bigGot[0].Text))
	fmt.Fprintf(out, "  %d octets crossed and came back identical, record %d\n", len(big), bigSent.RecordId)

	// ── 7 ────────────────────────────────────────────────────────────────────────────────
	step("B's client is KILLED mid-conversation and started again over the same directory")
	fmt.Fprintf(out, "  WHAT THIS IS FOR: this is S2-14. Everything B holds in memory is dropped -- the\n"+
		"  device, both durable stores, the transport and the connect client -- and a NEW everything\n"+
		"  is opened over the same directory. The only thing that crosses is the disk. If the MLS\n"+
		"  state did not survive, B is not in the group at all and the record below is undecryptable\n"+
		"  rather than merely unlisted.\n")
	const beforeTheRestart = "typed by A while B's app was about to be killed"
	if _, err := aGroup.Send(ctx, beforeTheRestart); err != nil {
		fail("A Send before B's restart: %v", err)
	}
	// AND TWO LINES OF B'S OWN, which is the half this step used to be blind to. It asserted
	// only that B could read what A sealed -- and `Receive` skipped every record whose
	// sender_handle was its own while a restored group's log started empty, so a user who
	// closed the app and reopened it got the other side's half of the conversation and none of
	// their own, with a nil error. A probe that checks only the far side's half cannot see that.
	bsOwn := []string{
		"typed by B ITSELF before the kill -- if this does not come back the user has lost their own half",
		"and a second line of B's own, before the kill",
	}
	for _, text := range bsOwn {
		if _, err := bGroup.Send(ctx, text); err != nil {
			fail("B Send before its own restart: %v", err)
		}
	}
	if _, err := aGroup.Receive(ctx); err != nil {
		fail("A Receive before B's restart: %v", err)
	}
	bHandle := append([]byte(nil), back[0].SenderHandle...)
	bEpoch := bGroup.Epoch()
	b = mesh.restart(b)
	defer b.close()
	// THIS IS THE CALL THAT MEETS THE RECONNECT WINDOW, every run: B has just re-dialled under
	// the same client_id. See connectAcrossTheReconnectWindow, which is where the sentence that
	// separates the operator's window from a broken restore lives, once.
	connectAcrossTheReconnectWindow(ctx, b, "the restarted B", "the restore itself")
	restored, err := b.device.Restore(ctx)
	if err != nil {
		fail("the restarted B Restore: %v", err)
	}
	check(len(restored) == 1, "B was in one group and %d came back from the disk", len(restored))
	bGroup = restored[0]
	check(string(bGroup.Id()) == string(groupId), "B came back into group %x and was in %x", bGroup.Id(), groupId)
	check(bGroup.Epoch() == bEpoch, "B was at epoch %d and came back at %d", bEpoch, bGroup.Epoch())
	check(bGroup.IsOpen(), "the restored group does not know it is open on the server, so it will refuse to send")
	// the cursor is not persisted, so this re-reads the whole history and re-derives every key
	afterRestart := receive(bGroup, ctx, "the restarted B")
	found := false
	for _, one := range afterRestart {
		if one.Text == beforeTheRestart {
			found = true
		}
	}
	check(found, "the restarted B read %d messages and none is the one A sealed before the restart", len(afterRestart))
	// B'S OWN HALF. The Mine bit is held separately from the text, because a device that came
	// back at a different leaf would open its own records as somebody else's and a check on the
	// text alone would pass over it.
	held := bGroup.Messages()
	for _, text := range bsOwn {
		mine := false
		for _, one := range held {
			if one.Text == text {
				mine = one.Mine
			}
		}
		check(mine, "the restarted B's log does not hold %q as its own; the user has lost their own half of the conversation. It holds %s",
			text, texts(held))
	}
	fmt.Fprintf(out, "  and B's own %d pre-restart line(s) came back as B's own, out of %d in its log\n",
		len(bsOwn), len(held))
	// THE PRE-CHANGE HISTORY COMES BACK. B was a member at epoch 1, so item 241 says every epoch-1
	// line is B's to read after the membership change: A's lines open under a REBUILT epoch-1
	// schedule (loaded once from the epoch-1 state blob the 32-epoch window keeps), and B's own come
	// from its local copies. Before 241 landed this block asserted the OPPOSITE -- exactly 602
	// out_of_window gaps -- and printed it as "out of this build's reach". Now the count that must be
	// exact is the other way round: ZERO gaps for a member who was there, and every A line from
	// epoch 1 counted under Stats.OpenedPastEpoch. A gap here is a line the change lost; an
	// OpenedPastEpoch below A's epoch-1 count is a line that came back some other way than the one
	// this build claims.
	check(bGroup.Epoch() == 2, "the restarted B's re-walk over the epoch commit left it at epoch %d, want 2", bGroup.Epoch())
	bGaps, bOpenedAfterRestart, bOtherGaps := gapCount(afterRestart)
	check(bOtherGaps == 0, "the restarted B's re-walk produced %d gap(s) of a reason other than out_of_window", bOtherGaps)
	check(bGaps == 0,
		"the restarted B re-walked %d epoch-one line(s) as out_of_window gaps; B was a member at epoch 1 and item 241 says it keeps every one of them",
		bGaps)
	check(bGroup.Stats().GapOutOfWindow == 0, "the restarted B's Stats.GapOutOfWindow is %d, want 0", bGroup.Stats().GapOutOfWindow)
	asEpochOne := uint64(epochOneLines - 1) // B sealed exactly one line at epoch 1 (step 3's answer); the rest are A's
	check(bGroup.Stats().OpenedPastEpoch == asEpochOne,
		"the restarted B opened %d record(s) under the rebuilt epoch-1 schedule and A sealed %d at epoch 1",
		bGroup.Stats().OpenedPastEpoch, asEpochOne)
	fmt.Fprintf(out, "  the restarted B re-walked its whole history at epoch 2 with 0 out_of_window gaps: %d of A's epoch-1\n"+
		"  line(s) opened under the REBUILT epoch-1 schedule (item 241, live), B's own came from copies, and\n"+
		"  %d line(s) in all opened with 0 failures\n", bGroup.Stats().OpenedPastEpoch, bOpenedAfterRestart)
	const afterTheRestart = "typed by the SAME device after it was restarted"
	if _, err := bGroup.Send(ctx, afterTheRestart); err != nil {
		fail("the restarted B Send: %v", err)
	}
	fromRestarted := receive(aGroup, ctx, "A")
	check(len(fromRestarted) == 1 && fromRestarted[0].Text == afterTheRestart,
		"A read %s for the one line the restarted B sent", texts(fromRestarted))
	check(string(fromRestarted[0].SenderHandle) == string(bHandle),
		"the restarted B seals under sender_handle %x and sealed under %x before, so it is a different leaf",
		fromRestarted[0].SenderHandle, bHandle)
	fmt.Fprintf(out, "  B came back at epoch %d under the same leaf, read %d messages including the one\n"+
		"  sealed before the restart, and A opened the one B sealed after it\n", bGroup.Epoch(), len(afterRestart))

	// ── 8 ────────────────────────────────────────────────────────────────────────────────
	step("two senders at once, which is where the stream indices and the reserver earn their keep")
	fmt.Fprintf(out, "  WHAT THIS IS FOR: each sender allocates from its OWN durable stream row, so two\n"+
		"  senders must never collide -- and a sender that raced with itself would reuse an index,\n"+
		"  which 5.6 calls a total break of both AEADs for that record. Every line below is\n"+
		"  distinct, so a lost one and a duplicated one are both visible in the counts.\n")
	const concurrent = 20
	var wait sync.WaitGroup
	sendErrors := make([]error, 2)
	fromA := map[string]bool{}
	fromB := map[string]bool{}
	for at := 0; at < concurrent; at += 1 {
		fromA[fmt.Sprintf("A concurrent %d", at)] = true
		fromB[fmt.Sprintf("B concurrent %d", at)] = true
	}
	wait.Add(2)
	go func() {
		defer wait.Done()
		for at := 0; at < concurrent; at += 1 {
			if _, err := aGroup.Send(ctx, fmt.Sprintf("A concurrent %d", at)); err != nil {
				sendErrors[0] = err
				return
			}
		}
	}()
	go func() {
		defer wait.Done()
		for at := 0; at < concurrent; at += 1 {
			if _, err := bGroup.Send(ctx, fmt.Sprintf("B concurrent %d", at)); err != nil {
				sendErrors[1] = err
				return
			}
		}
	}()
	wait.Wait()
	check(sendErrors[0] == nil, "A's concurrent sends failed: %v", sendErrors[0])
	check(sendErrors[1] == nil, "B's concurrent sends failed: %v", sendErrors[1])
	atA := receive(aGroup, ctx, "A")
	atB := receive(bGroup, ctx, "B")
	countOnce(atA, fromB, "A", "B")
	countOnce(atB, fromA, "B", "A")
	fmt.Fprintf(out, "  %d lines each way, concurrently: A opened %d of B's, B opened %d of A's, none twice\n",
		concurrent, len(atA), len(atB))

	// ── 9 ────────────────────────────────────────────────────────────────────────────────
	step("a reply, two reactions, one taken back, and a delete -- the content envelope over the mesh")
	fmt.Fprintf(out, "  WHAT THIS IS FOR: every line above this step was a TEXT. A reply, a reaction and a\n"+
		"  tombstone ride the SAME sealed body under a one-octet kind, so a build that got the\n"+
		"  envelope wrong does not refuse -- it renders them as garbage text attributed to a real\n"+
		"  sender. Every kind here is asserted on the FAR side, because the near side proves only\n"+
		"  that this build agrees with itself.\n")

	anchor, err := aGroup.Send(ctx, "the line every kind below points at")
	if err != nil {
		fail("A Send the anchor line: %v", err)
	}
	anchorAtB := findById(receive(bGroup, ctx, "B"), anchor.MessageId)
	check(anchorAtB != nil, "B never received the anchor line A sent")
	check(anchorAtB.Kind == urmessage.KindText,
		"the anchor came back as kind %s and A sealed it as a text", anchorAtB.Kind)

	// A REPLY carries a raw 32 octet reference in front of its text. The reference is the half a
	// text-only build cannot have got right by accident.
	const replyText = "a reply that names the line above"
	replySent, err := bGroup.SendReply(ctx, anchor.MessageId, replyText)
	if err != nil {
		fail("B SendReply: %v", err)
	}
	replyAtA := findById(receive(aGroup, ctx, "A"), replySent.MessageId)
	check(replyAtA != nil, "A never received B's reply")
	check(replyAtA.Kind == urmessage.KindReply, "B's reply came back as kind %s", replyAtA.Kind)
	check(bytes.Equal(replyAtA.ReplyToId, anchor.MessageId),
		"the reply names message %x and the anchor is %x", replyAtA.ReplyToId, anchor.MessageId)
	check(replyAtA.Text == replyText, "the reply's text came back as %q", replyAtA.Text)
	fmt.Fprintf(out, "  a REPLY crossed: it names the anchor's message_id and its text survived\n")

	// TWO REACTIONS, THEN ONE TAKEN BACK. A reaction adds no line of its own -- it changes a line
	// that is already there -- so the assertion is on the ANCHOR, not on what Receive answered.
	for _, emoji := range []string{"👍", "🎉"} {
		if _, err := bGroup.React(ctx, anchor.MessageId, emoji); err != nil {
			fail("B React %q: %v", emoji, err)
		}
	}
	receive(aGroup, ctx, "A")
	anchorAtA := findById(aGroup.Messages(), anchor.MessageId)
	check(anchorAtA != nil, "A lost its own anchor line out of its log")
	check(len(anchorAtA.Reactions) == 2,
		"A sees %d reaction(s) on its line and B sent two", len(anchorAtA.Reactions))

	if _, err := bGroup.Unreact(ctx, anchor.MessageId, "👍"); err != nil {
		fail("B Unreact: %v", err)
	}
	receive(aGroup, ctx, "A")
	anchorAtA = findById(aGroup.Messages(), anchor.MessageId)
	check(len(anchorAtA.Reactions) == 1,
		"after B took one back A sees %d reaction(s), want 1", len(anchorAtA.Reactions))
	check(anchorAtA.Reactions[0].Emoji == "🎉",
		"the reaction still standing is %q and the one taken back was the other",
		anchorAtA.Reactions[0].Emoji)
	fmt.Fprintf(out, "  two REACTIONS crossed and one was taken back: A sees exactly the one that stands\n")

	// THE SAME-SENDER RULE, ON THE SEND SIDE. R1 proves who wrote a TOMBSTONE and NOTHING proves
	// they wrote its target, so an honest build refuses to seal one naming somebody else's line.
	// This asserts the refusal, which is the arm a receiver-side check alone would leave untested.
	if _, err := bGroup.Delete(ctx, anchor.MessageId); err == nil {
		fail("B SEALED A TOMBSTONE FOR A'S MESSAGE. The same-sender rule is not enforced on the send\n" +
			"  side, so this build emits a record every honest receiver is supposed to ignore.")
	}
	fmt.Fprintf(out, "  and B REFUSED to delete A's line, which is the same-sender rule on the send side\n")

	if _, err := aGroup.Delete(ctx, anchor.MessageId); err != nil {
		fail("A Delete its own line: %v", err)
	}
	receive(bGroup, ctx, "B")
	anchorAtB = findById(bGroup.Messages(), anchor.MessageId)
	check(anchorAtB != nil, "B lost the anchor line entirely once it was deleted; it should be MARKED")
	check(anchorAtB.Deleted, "A deleted its line and B's copy of it is not marked deleted")
	fmt.Fprintf(out, "  a TOMBSTONE crossed: B's copy of A's line is marked deleted and still present\n")

	// ── 10 ───────────────────────────────────────────────────────────────────────────────
	step("roles: a promotion, a member's refused add, a transfer of ownership and a demotion, with every party's roster agreeing at every stage")
	fmt.Fprintf(out, "  WHAT THIS IS FOR: MASTER 11's role model (ledger item 242) is live on both arms --\n"+
		"  a commit the committer's role does not permit is refused before it is built and rejected\n"+
		"  by every receiver -- and this is the first time the two arms run over the deployed\n"+
		"  server with three real devices. Every stage prints each party's roster off its OWN\n"+
		"  Members() and asserts that the three agree, because a role only exists if every member\n"+
		"  reads the same one; and the refused add asserts that NO party's epoch moved, which is\n"+
		"  the send-side arm doing its job rather than the receivers cleaning up after it.\n")
	parties := []*namedGroup{{"A", aGroup}, {"B", bGroup}, {"C", cGroup}}
	// EVERY PARTY IS DRAINED FIRST. Steps 6 to 9 are conversations between A and B alone, so C
	// holds a backlog of every line since step 5, and the assertion below that a Receive after a
	// role commit hands back NO line is about the commit only if nothing else is waiting. The
	// first run of this step failed exactly here: C's Receive of the promotion answered 47 lines
	// it had simply not fetched yet.
	for _, one := range parties {
		backlog := receive(one.group, ctx, one.name)
		fmt.Fprintf(out, "  %s drained %d entr%s of backlog before the first role change\n", one.name, len(backlog),
			map[bool]string{true: "y", false: "ies"}[len(backlog) == 1])
	}
	identities := map[string]string{}
	for _, one := range parties {
		identities[string(identityOf(one.group, one.name))] = one.name
	}
	check(len(identities) == 3, "the three parties hold %d distinct identities", len(identities))
	bId, cId := identityOf(bGroup, "B"), identityOf(cGroup, "C")
	epochAtStart := aGroup.Epoch()
	rolesAgree("before any role change", parties, identities, epochAtStart,
		map[string]string{"A": "owner", "B": "member", "C": "member"})

	// STAGE 1: the owner promotes B. A commits and moves; B and C follow on their next Receive,
	// which hands back no line because a commit is not one.
	if err := aGroup.SetRole(ctx, bId, "admin"); err != nil {
		fail("A SetRole(B, admin): %v", err)
	}
	check(aGroup.Epoch() == epochAtStart+1, "A's promotion of B left A at epoch %d, want %d", aGroup.Epoch(), epochAtStart+1)
	for _, follower := range []*namedGroup{{"B", bGroup}, {"C", cGroup}} {
		got := receive(follower.group, ctx, follower.name)
		check(len(got) == 0, "%s's Receive of the promotion handed back %d entries and a commit is not a line: %s",
			follower.name, len(got), texts(got))
	}
	rolesAgree("A promoted B to admin", parties, identities, epochAtStart+1,
		map[string]string{"A": "owner", "B": "admin", "C": "member"})
	if role, err := bGroup.MyRole(); err != nil || role != "admin" {
		fail("B's MyRole after the promotion is %q, %v; want admin", role, err)
	}

	// STAGE 2: C, a MEMBER, tries to add a stranger. The send side refuses it with the
	// receivers' own sentence, nothing is built, and no party's epoch moves -- asserted on
	// every party after a Receive that finds nothing to ingest.
	stranger := mesh.stranger(c)
	defer stranger.close()
	strangerKeyPackage, err := stranger.device.KeyPackage()
	if err != nil {
		fail("the stranger's KeyPackage: %v", err)
	}
	refusedBefore := cGroup.Stats().CommitRefusedOwn
	_, err = cGroup.AddMemberAndPublish(ctx, strangerKeyPackage)
	check(err != nil, "C, a MEMBER, ADDED A STRANGER TO THE GROUP: the send-side arm of the role model is not running")
	check(errors.Is(err, urmessage.ErrCommitUnauthorized), "C's add was refused with %v, which does not wrap ErrCommitUnauthorized", err)
	check(errors.Is(err, urmessage.ErrCommitAddByNonAdmin), "C's add was refused with %v, which does not wrap R1's ErrCommitAddByNonAdmin", err)
	check(cGroup.Stats().CommitRefusedOwn == refusedBefore+1, "C's Stats.CommitRefusedOwn went %d -> %d over one refused add, want one more",
		refusedBefore, cGroup.Stats().CommitRefusedOwn)
	fmt.Fprintf(out, "  C's AddMemberAndPublish as a member was refused on the SEND side: %v\n", err)
	for _, one := range []*namedGroup{{"A", aGroup}, {"B", bGroup}} {
		got := receive(one.group, ctx, one.name)
		check(len(got) == 0, "%s fetched %d entries after C's refused add; something was published: %s", one.name, len(got), texts(got))
	}
	rolesAgree("C's add of a stranger was refused, nothing moved", parties, identities, epochAtStart+1,
		map[string]string{"A": "owner", "B": "admin", "C": "member"})

	// STAGE 3: A hands the group to B. B is the owner and A is an admin from then (ruling 4).
	if err := aGroup.TransferOwnership(ctx, bId); err != nil {
		fail("A TransferOwnership(B): %v", err)
	}
	check(aGroup.Epoch() == epochAtStart+2, "the transfer left A at epoch %d, want %d", aGroup.Epoch(), epochAtStart+2)
	for _, follower := range []*namedGroup{{"B", bGroup}, {"C", cGroup}} {
		got := receive(follower.group, ctx, follower.name)
		check(len(got) == 0, "%s's Receive of the transfer handed back %d entries: %s", follower.name, len(got), texts(got))
	}
	rolesAgree("A transferred ownership to B", parties, identities, epochAtStart+2,
		map[string]string{"A": "admin", "B": "owner", "C": "member"})
	if role, _ := bGroup.MyRole(); role != "owner" {
		fail("B's MyRole after the transfer is %q, want owner", role)
	}
	if role, _ := aGroup.MyRole(); role != "admin" {
		fail("A's MyRole after the transfer is %q, want admin", role)
	}

	// STAGE 4: the new owner demotes C to observer, and A -- now an admin -- follows a commit
	// it did not make.
	if err := bGroup.SetRole(ctx, cId, "observer"); err != nil {
		fail("B SetRole(C, observer) as the new owner: %v", err)
	}
	check(bGroup.Epoch() == epochAtStart+3, "B's demotion of C left B at epoch %d, want %d", bGroup.Epoch(), epochAtStart+3)
	for _, follower := range []*namedGroup{{"A", aGroup}, {"C", cGroup}} {
		got := receive(follower.group, ctx, follower.name)
		check(len(got) == 0, "%s's Receive of the demotion handed back %d entries: %s", follower.name, len(got), texts(got))
	}
	rolesAgree("B, the new owner, demoted C to observer", parties, identities, epochAtStart+3,
		map[string]string{"A": "admin", "B": "owner", "C": "observer"})
	if role, _ := cGroup.MyRole(); role != "observer" {
		fail("C's MyRole after the demotion is %q, want observer", role)
	}
	fmt.Fprintf(out, "  four role commits crossed the mesh and three rosters agreed at every one of them\n")

	// ── 11 ───────────────────────────────────────────────────────────────────────────────
	step("REMOVAL: the owner takes a member's TWO devices out in ONE commit, a survivor that was " +
		"offline across it converges, and the removed device is told so and stays told")
	fmt.Fprintf(out, "  WHAT THIS IS FOR: this is the removal track (ledger items 257-259) over the deployed\n"+
		"  server. Everything the track built is held in-process -- the derivation and the refusals in\n"+
		"  urmessage, the submit and the convergence in cp3b's\n"+
		"  TestOneCallRemovesEveryLeafOfOneIdentityAndTheRemovedMemberCannotFollow and\n"+
		"  TestARemovedDeviceIsToldSoByNameOnEveryWalkAndStillIsAfterARestart -- and none of it has\n"+
		"  ever crossed an operator's mesh. Six properties are asserted here and each has a way to\n"+
		"  fail that a one-process test cannot see: a real fetch page under item 246's epoch ceiling,\n"+
		"  a real 60-second reconnect window between a party going offline and coming back, and a\n"+
		"  real disk between the removal and the restart that must still answer `you were removed`.\n")

	// THE EPOCH EVERY CLAUSE BELOW IS RELATIVE TO, and the parties as step 10 left them: A an
	// ADMIN, B the OWNER, C an OBSERVER. It is asserted rather than assumed because the whole of
	// this step's arithmetic hangs off it.
	removalBase := epochAtStart + 3
	check(aGroup.Epoch() == removalBase && bGroup.Epoch() == removalBase && cGroup.Epoch() == removalBase,
		"step 11 starts with A at %d, B at %d and C at %d, and step 10 left all three at %d",
		aGroup.Epoch(), bGroup.Epoch(), cGroup.Epoch(), removalBase)
	aId := identityOf(aGroup, "A")

	// ── STAGE 0: THE DIAL-TIME SEED CHECK'S OWN CONTROL, BOTH WAYS ─────────────────────────────
	//
	// dial() refuses a party whose state directory predates the X-Wing wrap seed, and the whole of
	// that refusal is one sentinel. A check written that way is worth exactly what its two arms are
	// worth, so both are driven here, against devices this run already holds and with no old
	// directory needed.
	//
	// THE YES ARM: a device whose seed has been ERASED answers the sentinel. Device.Close erases it
	// in place and sets it nil precisely so that a decapsulation afterwards refuses by name rather
	// than expanding 32 zero octets into a well formed key and answering a wrong secret that looks
	// exactly like a right one -- and that is the SAME empty field a three-part store leaves behind.
	// The store's own arm is held in urmessage by
	// TestAStoreWrittenBeforeTheWrapSeedRestoresWithItsOwnLeafAndRefusesByName, whose control is that
	// the same construction WITH a seed opens the encapsulation.
	//
	// THE NO ARM: a live party answers a DIFFERENT error to the same empty ciphertext, so the refusal
	// at dial time separates a missing seed from a bad argument rather than firing for everything.
	// The stranger is step 10's and has done its job; closing it here is the cheapest erased device
	// this run can produce, and cp3b's
	// TestEachLeafsPublishedWrapKeyIsOpenedByThatLeafsOwnDeviceAndNoOther drives the same pair.
	stranger.device.Close()
	_, erasedSeed := stranger.device.DecapsulateToOwnLeaf(nil)
	check(errors.Is(erasedSeed, urmessage.ErrNoDeviceWrapKey),
		"CONTROL FAILED: a device whose wrap seed has been erased answered %v to a decapsulation, want "+
			"ErrNoDeviceWrapKey. dial()'s refusal of a pre-seed state directory is that sentinel and "+
			"nothing else, so a no here means the refusal can never fire -- and a three-part directory "+
			"would then reach step 11 and satisfy `it cannot derive the epoch` vacuously", erasedSeed)
	_, liveSeed := b.device.DecapsulateToOwnLeaf(nil)
	check(liveSeed != nil && !errors.Is(liveSeed, urmessage.ErrNoDeviceWrapKey),
		"CONTROL FAILED: a live party holding its own seed answered %v to the same empty ciphertext. The "+
			"arm above is a discriminator only if this one is a different answer: the same sentinel would "+
			"mean dial() refuses every party, and a nil would mean the check reads a success as an absence",
		liveSeed)
	fmt.Fprintf(out, "  the seed check separates its two arms: erased -> %v; live -> %v\n", erasedSeed, liveSeed)

	// ── STAGE 1: THE VICTIM IS MADE A MEMBER, BECAUSE A MEMBER IS WHO GETS REMOVED ─────────────
	//
	// Step 10 left C an OBSERVER. §11's table gives an ADMIN "remove MEMBERs and OBSERVERs", so
	// either would do for R2 -- but the refusal in stage 4 has to be a MEMBER's by name, and the
	// victim's own Send in stage 7 has to be refused for the REMOVAL and not for being an
	// observer's. A member-to-observer change and back is an admin's or the owner's (ruling 15's
	// second paragraph), and B is the owner.
	if err := bGroup.SetRole(ctx, cId, "member"); err != nil {
		fail("B SetRole(C, member) as the owner, putting the victim back to a member: %v", err)
	}
	for _, follower := range []*namedGroup{{"A", aGroup}, {"C", cGroup}} {
		got := receive(follower.group, ctx, follower.name)
		check(len(got) == 0, "%s's Receive of C's promotion back to member handed back %d entries: %s",
			follower.name, len(got), texts(got))
	}
	rolesAgree("C was made a member again, so the removal below is of a MEMBER", parties, identities,
		removalBase+1, map[string]string{"A": "admin", "B": "owner", "C": "member"})

	// ── STAGE 2: THE VICTIM GETS A SECOND DEVICE, AND COMMITS THAT ADD ITSELF ───────────────────
	//
	// WHY THE STEP NEEDS IT. Ruling 49's whole content is "one identity-keyed call removes ALL of
	// that identity's leaves in one commit", and an identity with one leaf cannot tell that verb
	// from a per-leaf one. A removal that took one of somebody's two devices has removed nobody: the
	// leaf left standing reads every epoch and writes to the group. So the victim is given a second
	// leaf and the assertion is that BOTH go, in ONE commit.
	//
	// AND IT IS A LEAF AND NOT A FOURTH urmessage.Device, WHICH IS A REAL LIMIT AND IS STATED. A
	// Device's credential identity IS its signature key (device.go says so in as many words), and
	// one Device mints one identity per state store, so there is no door in that package onto a
	// SECOND leaf of an EXISTING identity. What can carry one is the seam: a key package whose
	// credential names C's identity and whose signer is its own, which is exactly what cp3b's
	// world.seamMemberClaiming builds and what its removal case is driven with. THAT IS NOT A
	// FORGERY WHEN THAT IDENTITY COMMITS THE ADD: §11's self-service rule gives every member its own
	// device leaves, and R6a requires an Add claiming an identity already in the group to be
	// committed BY that identity -- so C's own AddMemberAndPublish is the only call that can land it,
	// and the same key package offered by A or B is refused at every receiver.
	//
	// WHAT IT CANNOT DO, AND THE STEP DOES NOT PRETEND OTHERWISE: it never connects and never
	// fetches. A fourth reading party would need a fourth credential, and this deployment has three
	// accounts. What the step needs from it is that it OCCUPIES A LEAF -- which is what the removal
	// vector is about -- and every reading assertion below is made at a party that really reads.
	laptop := secondLeafFor("C's second device", cId)
	epochBeforeTheLaptop := cGroup.Epoch()
	if _, err := cGroup.AddMemberAndPublish(ctx, laptop); err != nil {
		fail("C's AddMemberAndPublish of its OWN second device: %v\n"+
			"  A MEMBER may add its own device leaves and nothing else (MASTER 11's self-service rule,\n"+
			"  R7), and an Add claiming an identity already in the group must be committed by that\n"+
			"  identity (R6a). If this is ErrCommitUnauthorized then one of those two rules is refusing\n"+
			"  the case they exist to allow; if it is a seam refusal the key package's credential does\n"+
			"  not name C's identity and the whole of this step is about the wrong leaf", err)
	}
	check(cGroup.Epoch() == epochBeforeTheLaptop+1,
		"C's add of its own second device left C at epoch %d, want %d", cGroup.Epoch(), epochBeforeTheLaptop+1)
	for _, follower := range []*namedGroup{{"A", aGroup}, {"B", bGroup}} {
		got := receive(follower.group, ctx, follower.name)
		check(len(got) == 0, "%s's Receive of C's own-device add handed back %d entries: %s",
			follower.name, len(got), texts(got))
	}
	rostersAgree("C's own second device took a leaf", parties, identities, removalBase+2,
		map[string]string{"A": "admin", "B": "owner", "C": "member"}, 4)
	// THE CONTROL THE WHOLE STEP RESTS ON, at every party that can read a roster: C's identity holds
	// TWO leaves and each survivor holds ONE. Without the second half, "both of C's leaves are gone"
	// below is satisfied by a roster that lost everybody.
	for _, one := range parties {
		if got := leavesOf(one.group, cId, one.name); len(got) != 2 {
			fail("CONTROL FAILED: %s reads C's identity at %d leaf/leaves (%v), and this step is about "+
				"ONE call that takes EVERY leaf of an identity", one.name, len(got), got)
		}
		for who, identity := range map[string][]byte{"A": aId, "B": bId} {
			if got := leavesOf(one.group, identity, one.name); len(got) != 1 {
				fail("CONTROL FAILED: %s reads %s at %d leaf/leaves, want 1", one.name, who, len(got))
			}
		}
	}
	victimLeaves := leavesOf(bGroup, cId, "B")
	fmt.Fprintf(out, "  C's identity holds leaves %v at every party, and A and B hold one each\n", victimLeaves)

	// ── STAGE 3: THE SURVIVOR IS DEMOTED TO MEMBER, SO THE REFUSAL BELOW IS A MEMBER'S ─────────
	if err := bGroup.SetRole(ctx, aId, "member"); err != nil {
		fail("B SetRole(A, member) as the owner: %v", err)
	}
	for _, follower := range []*namedGroup{{"A", aGroup}, {"C", cGroup}} {
		got := receive(follower.group, ctx, follower.name)
		check(len(got) == 0, "%s's Receive of A's demotion handed back %d entries: %s",
			follower.name, len(got), texts(got))
	}
	rostersAgree("A was demoted to member", parties, identities, removalBase+3,
		map[string]string{"A": "member", "B": "owner", "C": "member"}, 4)
	if role, err := aGroup.MyRole(); err != nil || role != "member" {
		fail("A's MyRole after the demotion is %q, %v; want member", role, err)
	}

	// ── STAGE 4: THE SEND-SIDE REFUSALS, WHILE THE VICTIM IS STILL A MEMBER ────────────────────
	//
	// FOUR REQUESTS, AND THE ASSERTION THAT MATTERS IS THAT NOTHING WAS BUILT. A refusal that fired
	// after the commit was submitted would leave the group at a new epoch with a commit every
	// receiver then refuses -- which item 242's cost paragraph calls halting the group. So every
	// party is fetched afterwards and must find NOTHING: no entry, no epoch move, no ingest, and no
	// receiving-side refusal either, because a receiving-side refusal is evidence that something
	// reached the wire.
	//
	// THE ORDER OF THE ANSWERS IS RULING 55's AND IT IS ASSERTED, not merely relied on. A MEMBER
	// asking to remove the OWNER is answered "there is no removing the owner", not "you may not
	// remove": the subject-level doors come BEFORE the predicate because they are the requests for
	// which no commit exists at all, whoever asks. That is the opposite of SetRole's order and the
	// difference is principled (ruling 15's caller check exists because the predicate cannot decide
	// its case; here the predicate decides every authority question). A build that put the predicate
	// first would answer R2 here and would still be green on every other clause in this step.
	ingestedBeforeRefusals := map[string]uint64{}
	receivedRefusalsBefore := map[string]uint64{}
	for _, one := range parties {
		ingestedBeforeRefusals[one.name] = one.group.Stats().Ingested
		receivedRefusalsBefore[one.name] = one.group.Stats().CommitRefused
	}
	ownRefusedBefore := aGroup.Stats().CommitRefusedOwn

	// R2: a MEMBER may not remove another member
	refusal := aGroup.RemoveMember(ctx, cId)
	check(refusal != nil,
		"A, a MEMBER, REMOVED C FROM THE GROUP: the send-side arm of the role model is not running")
	check(errors.Is(refusal, urmessage.ErrCommitUnauthorized),
		"A's removal was refused with %v, which does not wrap ErrCommitUnauthorized", refusal)
	check(errors.Is(refusal, urmessage.ErrCommitRemoveByNonAdmin),
		"A's removal was refused with %v, which does not wrap R2's ErrCommitRemoveByNonAdmin", refusal)
	check(aGroup.Stats().CommitRefusedOwn == ownRefusedBefore+1,
		"A's Stats.CommitRefusedOwn went %d -> %d over one rule refusal, want one more",
		ownRefusedBefore, aGroup.Stats().CommitRefusedOwn)
	fmt.Fprintf(out, "  A's RemoveMember as a MEMBER was refused on the SEND side: %v\n", refusal)

	// AND THE THREE BY-NAME DOORS, which are about the SUBJECT and are true whoever asks
	refusal = aGroup.RemoveMember(ctx, bId)
	check(errors.Is(refusal, urmessage.ErrRemoveOwner),
		"A removing the OWNER answered %v, want ErrRemoveOwner", refusal)
	check(!errors.Is(refusal, urmessage.ErrCommitRemoveByNonAdmin),
		"A, a MEMBER, removing the OWNER was answered R2's `only an admin or the owner may remove`: %v.\n"+
			"  RULING 55 puts the subject-level doors BEFORE the predicate, so the answer owed here is\n"+
			"  `nobody removes the owner, transfer first` -- which names the door -- and not a sentence\n"+
			"  that invites a member to think some admin could", refusal)
	refusal = aGroup.RemoveMember(ctx, aId)
	check(errors.Is(refusal, urmessage.ErrRemoveSelf),
		"A removing its own identity answered %v, want ErrRemoveSelf", refusal)
	refusal = aGroup.RemoveMember(ctx, bytes.Repeat([]byte{0x11}, len(aId)))
	check(errors.Is(refusal, urmessage.ErrNoSuchMember),
		"A removing an identity that holds no leaf answered %v, want ErrNoSuchMember", refusal)
	check(aGroup.Stats().CommitRefusedOwn == ownRefusedBefore+1,
		"a by-name door was counted as a role refusal: A's CommitRefusedOwn is %d, want %d. The three "+
			"doors are a caller asking for something that does not exist, not a role model refusing an "+
			"authority", aGroup.Stats().CommitRefusedOwn, ownRefusedBefore+1)

	// NOTHING REACHED THE WIRE, AT ANY PARTY
	for _, one := range parties {
		got := receive(one.group, ctx, one.name)
		check(len(got) == 0, "%s fetched %d entr%s after four refused removals; something was published: %s",
			one.name, len(got), map[bool]string{true: "y", false: "ies"}[len(got) == 1], texts(got))
		check(one.group.Epoch() == removalBase+3, "%s is at epoch %d after four refused removals, want %d",
			one.name, one.group.Epoch(), removalBase+3)
		check(one.group.Stats().Ingested == ingestedBeforeRefusals[one.name],
			"%s ingested a commit after the refused removals (%d -> %d)",
			one.name, ingestedBeforeRefusals[one.name], one.group.Stats().Ingested)
		check(one.group.Stats().CommitRefused == receivedRefusalsBefore[one.name],
			"%s counted a RECEIVING-side refusal (%d -> %d), which is evidence that a commit the send "+
				"side was supposed to refuse reached the server", one.name,
			receivedRefusalsBefore[one.name], one.group.Stats().CommitRefused)
	}
	rostersAgree("four removals were refused on the send side and nothing moved", parties, identities,
		removalBase+3, map[string]string{"A": "member", "B": "owner", "C": "member"}, 4)

	// ── STAGE 5: A SURVIVOR GOES OFFLINE, ACROSS THE REMOVAL ───────────────────────────────────
	//
	// EVERYTHING A IS HOLDING IS DROPPED AND ITS DIRECTORY IS LEFT ALONE, which is what the
	// operating system does when the user closes the app -- and then the group changes underneath
	// it. This is the case every real group chat meets on the first day and no in-process test can:
	// the member that was not there. What it must NOT be is a member that comes back agreeing with
	// nobody, which is a roster that names somebody who is gone and a key schedule it cannot follow.
	aWasAtEpoch := aGroup.Epoch()
	fmt.Fprintf(out, "  taking A OFFLINE at epoch %d, across the removal\n", aWasAtEpoch)
	mesh.kill(a)
	// NOTHING BELOW TOUCHES aGroup UNTIL STAGE 11 REPLACES IT. The group behind it is closed, so a
	// roster read through it would answer "this group is closed" rather than a membership -- which is
	// why the stage 6 roster assertion names only B.

	// ── STAGE 6: THE REMOVAL. ONE CALL, BY IDENTITY, BOTH LEAVES ───────────────────────────────
	victimEpoch := cGroup.Epoch()
	leavesBeforeTheRemoval := leavesOf(bGroup, cId, "B")
	check(len(leavesBeforeTheRemoval) == 2, "CONTROL FAILED: C holds %d leaf/leaves at the remover on "+
		"the line before the removal", len(leavesBeforeTheRemoval))
	if err := bGroup.RemoveMember(ctx, cId); err != nil {
		fail("B's RemoveMember of C, as the OWNER removing a MEMBER: %v", err)
	}
	openedEpoch := victimEpoch + 1
	check(bGroup.Epoch() == openedEpoch, "ONE removal commit left B at epoch %d, want %d",
		bGroup.Epoch(), openedEpoch)
	check(len(leavesOf(bGroup, cId, "B")) == 0,
		"B still reads C's identity at leaves %v after the removal, and C held %v: a removal that "+
			"leaves one of somebody's devices in the group has removed nobody",
		leavesOf(bGroup, cId, "B"), leavesBeforeTheRemoval)
	for who, identity := range map[string][]byte{"A": aId, "B": bId} {
		check(len(leavesOf(bGroup, identity, "B")) == 1,
			"CONTROL FAILED: B reads %s at %d leaf/leaves after the removal, want 1: the absence above "+
				"is satisfied by a roster that lost everybody", who, len(leavesOf(bGroup, identity, "B")))
	}
	// AND THE POLICY ENTRY WENT WITH THE LEAVES, which is what the roster's ROLES say. A commit that
	// left C named would be an R0c phantom every honest receiver refuses, so A's convergence in
	// stage 11 is the other half of this; what is read here is that the two survivors' own roles
	// came through the wholesale GroupContextExtensions replacement intact (item 242's P4).
	rostersAgree("the OWNER removed BOTH of C's leaves in one commit", []*namedGroup{{"B", bGroup}},
		identities, openedEpoch, map[string]string{"A": "member", "B": "owner"}, 2)
	fmt.Fprintf(out, "  one call took C's %d leaves %v and its policy entry out, and opened epoch %d\n",
		len(leavesBeforeTheRemoval), leavesBeforeTheRemoval, openedEpoch)

	// ── STAGE 7: THE REMOVED DEVICE'S OWN CLIENT, AND IT IS TOLD BY NAME ───────────────────────
	//
	// WHAT THIS REPLACED WAS MEASURED, not imagined (ledger item 259): before ruling 52's carrier a
	// removed client answered the sentinel on walk one, a consumed-ratchet error on walk two,
	// ErrRecordAbandoned on walk three, and NIL FOR EVER after that -- so a device that had been
	// thrown out of a group read as caught up and silent, indistinguishable from a quiet room.
	//
	// Stats.FailedOpen IS THE DISCRIMINATOR AND Stats.Unopened IS NOT, which cp3b measured rather
	// than reasoned: the sticky clause means only one attempt is ever spent, so nothing is ever
	// abandoned and Unopened stays 0 even over a walk that did treat the removal as a record that
	// did not open. The counter that moves on the FIRST attempt is the one to assert.
	failedOpenBefore := cGroup.Stats().FailedOpen
	fetched := []uint64{}
	for walk := 1; walk <= 2; walk += 1 {
		_, err := cGroup.Receive(ctx)
		assertRemoved(fmt.Sprintf("C's walk %d", walk), err)
		fetched = append(fetched, cGroup.Stats().Fetched)
		check(cGroup.Stats().FailedOpen == failedOpenBefore,
			"after walk %d the removed device has spent %d open attempt(s) on the removing commit "+
				"(FailedOpen %d -> %d): a removal is the one record a device cannot open and must not retry",
			walk, cGroup.Stats().FailedOpen-failedOpenBefore, failedOpenBefore, cGroup.Stats().FailedOpen)
		check(cGroup.Stats().Unopened == 0 && len(cGroup.UnopenedRecords()) == 0,
			"after walk %d the removed device holds %d unopened record(s) %v: the removing commit was abandoned",
			walk, cGroup.Stats().Unopened, cGroup.UnopenedRecords())
		check(cGroup.Epoch() == victimEpoch,
			"after walk %d the removed device is at epoch %d, want %d: it followed its own removal",
			walk, cGroup.Epoch(), victimEpoch)
		epoch, state := cGroup.Removal()
		check(state != nil && epoch == victimEpoch,
			"after walk %d the removed device's Removal answers (%d, %v), want (%d, non-nil)",
			walk, epoch, state, victimEpoch)
	}
	_, refusal = cGroup.Send(ctx, "a line from a device this group removed")
	assertRemoved("the removed device's Send", refusal)
	// AND IT STILL READS WHAT IT IS ENTITLED TO. It holds the keys of the epoch it was removed at,
	// so the transcript up to that epoch is exactly what Spec C's read-only screen renders. The line
	// checked is the one C sealed ITSELF at epoch 2, in step 5.
	heldAtVictim := textsPresent(cGroup.Messages())
	check(heldAtVictim[cAtTwo],
		"the removed device's log has lost the line it sent itself at epoch 2. It holds the keys of "+
			"the epoch it was removed at, so its whole transcript up to that epoch still reads")

	// ── STAGE 8: THE SURVIVOR SEALS ABOVE THE CEILING ──────────────────────────────────────────
	ceiling := []string{}
	for at := 0; at < removalCeilingLines; at += 1 {
		ceiling = append(ceiling, fmt.Sprintf(
			"line %d of %d sealed by B at epoch %d, above the epoch C was removed at",
			at+1, removalCeilingLines, openedEpoch))
	}
	for _, line := range ceiling {
		if _, err := bGroup.Send(ctx, line); err != nil {
			fail("B's Send above the ceiling: %v", err)
		}
	}

	// ── STAGE 9: ITEM 246's CEILING, MEASURED ON THE SERVER'S OWN PAGES ───────────────────────
	//
	// WHAT IS BEING ASSERTED. F0 (item 246) has the store return only rows whose epoch is at or
	// below the read_epoch inside the request's own req_auth. The removed device's read_epoch is the
	// epoch it was removed AT, so the rows B just sealed at the epoch the removal OPENED must not be
	// served to it at all -- which is a stronger statement than "it cannot open them", and is the
	// difference between a removed member that is denied this server's rows and one that is merely
	// denied the keys.
	//
	// THE INSTRUMENT IS Stats.Fetched AND THE TEXT CHECK BELOW IS NOT, which is the honest
	// complement: the removed device could not open those lines whether or not they were served, so
	// a log that does not hold them holds nothing about the ceiling. A blocked walk re-fetches the
	// same fixed set of rows every time -- it stops at the removing commit and the cursor never
	// moves past it -- so the delta per walk is a constant, and the question is whether the walk
	// that spans B's new rows fetched MORE than the walks that did not.
	//
	// AND THE CONTROL IS THE OTHER WAY ROUND AND IS IN STAGE 11: the returning A opens every one of
	// these lines. Without it, a zero here is satisfied by a server that served those rows to
	// NOBODY, which is an omission and not a ceiling.
	perWalk := fetched[1] - fetched[0]
	check(0 < perWalk,
		"CONTROL FAILED: the removed device's two blocked walks fetched %d and then %d records in "+
			"total, so the per-walk delta is zero and the comparison below cannot distinguish a "+
			"ceiling from a client that stopped fetching", fetched[0], fetched[1])
	for walk := 3; walk <= 4; walk += 1 {
		_, err := cGroup.Receive(ctx)
		assertRemoved(fmt.Sprintf("C's walk %d", walk), err)
		fetched = append(fetched, cGroup.Stats().Fetched)
	}
	for _, walk := range []int{2, 3} {
		check(fetched[walk]-fetched[walk-1] == perWalk,
			"ITEM 246's CEILING: the removed device's walk %d fetched %d records where the walks before "+
				"B sealed anything fetched %d each. The %d line(s) B sealed at epoch %d were SERVED to a "+
				"reader whose authenticated read_epoch is %d, so the store is applying no epoch filter. "+
				"If this server predates F0 that is the finding and not a client defect",
			walk+1, fetched[walk]-fetched[walk-1], perWalk, len(ceiling), openedEpoch, victimEpoch)
	}
	heldAtVictim = textsPresent(cGroup.Messages())
	for _, line := range ceiling {
		check(!heldAtVictim[line],
			"the removed device's log holds %q, which was sealed at the epoch its own removal opened "+
				"and which it holds no storage_root for", line)
	}
	check(cGroup.Epoch() == victimEpoch,
		"the removed device is at epoch %d after four walks over the epoch its removal opened, want %d",
		cGroup.Epoch(), victimEpoch)
	fmt.Fprintf(out, "  the removed device answered the removal by name on 4 walks and a send, kept its own\n"+
		"  epoch-2 line, and was served %d record(s) per walk both before and after B sealed %d line(s)\n"+
		"  above the ceiling -- so item 246 held on the server's own pages\n", perWalk, len(ceiling))

	// ── STAGE 10: THE REMOVED DEVICE IS RESTARTED OVER THE SAME DIRECTORY ─────────────────────
	//
	// THE ONE READING A RE-DERIVATION CANNOT FAKE is taken BEFORE the first Receive of the new
	// process. The receive cursor is not persisted, so the removing commit is still sitting above a
	// cursor at zero; a removal state read here came off the group record's own field on the disk and
	// from nowhere else. Without ruling 52's persist, such a device comes back reading as caught up
	// and silent until some walk happens to re-derive it, which is the state the ruling exists to end.
	c = mesh.restart(c)
	defer c.close()
	connectAcrossTheReconnectWindow(ctx, c, "the restarted C", "the removed device's persisted state")
	restoredVictim, err := c.device.Restore(ctx)
	if err != nil {
		fail("the restarted C's Restore: %v\n"+
			"  A device a commit removed is still a device whose own history is on this disk, so a "+
			"restore that refused the group would take the transcript away with the membership", err)
	}
	check(len(restoredVictim) == 1, "the restarted C restored %d group(s), want 1", len(restoredVictim))
	cGroup = restoredVictim[0]
	removedAtEpoch, removalState := cGroup.Removal()
	check(removalState != nil,
		"the restored group reads (%d, nil) from Removal BEFORE its first Receive: ruling 52's state "+
			"did not survive the process, so this device comes back reading as caught up and silent",
		removedAtEpoch)
	check(removedAtEpoch == victimEpoch,
		"the restored group says it was removed at epoch %d, want %d", removedAtEpoch, victimEpoch)
	check(errors.Is(removalState, urmessage.ErrRemovedFromGroup) && errors.Is(removalState, mls.ErrRemovedFromGroup),
		"the restored state is %v: it must carry urmessage.ErrRemovedFromGroup AND mls's own sentinel, "+
			"because the cause is a value and not state a restart can invalidate", removalState)
	check(cGroup.Epoch() == victimEpoch,
		"the restored group is at epoch %d, want %d", cGroup.Epoch(), victimEpoch)
	for walk := 1; walk <= 2; walk += 1 {
		_, err := cGroup.Receive(ctx)
		assertRemoved(fmt.Sprintf("C's walk %d after the restart", walk), err)
	}
	_, refusal = cGroup.Send(ctx, "a line from a device this group removed, after a restart")
	assertRemoved("the removed device's Send after the restart", refusal)
	check(cGroup.Stats().Unopened == 0,
		"the restarted removed device holds %d unopened record(s) after re-walking its whole history, want 0",
		cGroup.Stats().Unopened)
	fmt.Fprintf(out, "  and it came back from a restart already knowing, before its first fetch: removed at epoch %d\n",
		removedAtEpoch)

	// ── STAGE 11: THE SURVIVOR THAT WAS OFFLINE ACROSS THE REMOVAL COMES BACK ─────────────────
	a = mesh.dial("A", mesh.jwtOf("A"))
	defer a.close()
	connectAcrossTheReconnectWindow(ctx, a, "the returning A", "the offline survivor's convergence")
	restoredSurvivor, err := a.device.Restore(ctx)
	if err != nil {
		fail("the returning A's Restore: %v", err)
	}
	check(len(restoredSurvivor) == 1, "the returning A restored %d group(s), want 1", len(restoredSurvivor))
	aGroup = restoredSurvivor[0]
	check(aGroup.Epoch() == aWasAtEpoch,
		"the returning A came back at epoch %d and was at %d when it went offline: the disk did not "+
			"hold the epoch, so nothing below is about a member catching up",
		aGroup.Epoch(), aWasAtEpoch)
	// IT DRAINS, AND THE UNIT IS THE ROUND TRIP RATHER THAN THE WALK. Under item 246's ceiling one
	// Receive crosses one epoch and stops -- the reasoning is on [receiveAcrossTheEpochCeiling] -- so
	// a party that must cross the epoch the removal opened AND read the lines B sealed above it needs
	// more than one, and a stage built on one would have gone red naming the server.
	backFromOffline, aRounds := receiveAcrossTheEpochCeiling(aGroup, ctx, "the returning A")
	check(aRounds[0].epoch == openedEpoch,
		"the returning A is at epoch %d after its FIRST round trip, want %d: it did not follow the "+
			"removal it slept through. Exactly one commit sits above the epoch its disk restored, and "+
			"item 246 serves one epoch per round trip, so crossing it is the FIRST round's whole job "+
			"and reading what was sealed above it is a later round's", aRounds[0].epoch, openedEpoch)
	check(aGroup.Epoch() == openedEpoch,
		"the returning A settled at epoch %d after %d round trip(s), want %d",
		aGroup.Epoch(), len(aRounds), openedEpoch)
	check(aGroup.Stats().Ingested == 1,
		"the returning A ingested %d commit(s) over %d round trip(s). Exactly one commit sits above "+
			"the epoch its disk restored -- the removal -- and every commit below has a header epoch "+
			"this session is past, which is ceremony a walk reads over. Any other number means the "+
			"re-walk re-ingested commits it had already applied, and holding it over a DRAIN is the "+
			"stronger reading: a round trip that re-applied anything shows up here first",
		aGroup.Stats().Ingested, len(aRounds))
	check(0 < aGroup.Stats().WrapOpened,
		"the returning A opened %d device wrap(s) over a walk that took it into a new epoch. The "+
			"epoch a removal opens is reached ONLY by opening the fan-out wrap addressed to this "+
			"leaf, so a zero here means it arrived at the epoch some other way", aGroup.Stats().WrapOpened)
	check(aGroup.Stats().GapMalformed == 0,
		"the returning A re-walked its history with %d malformed gap(s). The removed member's records "+
			"sit BELOW the removing commit and are its whole half of the conversation; a member that "+
			"cannot resolve them after the removal has lost them, which is ledger item 259's defect one "+
			"arm over", aGroup.Stats().GapMalformed)
	check(aGroup.Stats().GapOutOfWindow == 0,
		"the returning A re-walked %d line(s) as out_of_window gaps. It was a member from epoch 1 and "+
			"item 241 says it keeps every one of them", aGroup.Stats().GapOutOfWindow)
	check(aGroup.Stats().FailedOpen == 0,
		"the returning A failed to open %d record(s) across the removal", aGroup.Stats().FailedOpen)
	check(0 < aGroup.Stats().OpenedPastEpoch,
		"the returning A opened %d record(s) under a rebuilt past-epoch schedule over a re-walk that "+
			"spans %d epochs, so the zero gap count above is not about history it actually read",
		aGroup.Stats().OpenedPastEpoch, openedEpoch)
	// THE CONTROL FOR STAGE 9, AND IT IS THE OTHER WAY ROUND: the rows DO exist on the server and a
	// member at the epoch they were sealed at gets every one of them.
	openedAfterOffline := textsPresent(backFromOffline)
	for _, line := range ceiling {
		check(openedAfterOffline[line],
			"the returning A did not open %q over %d round trip(s), which B sealed at epoch %d. Without "+
				"this the ceiling measurement in stage 9 is satisfied by a server that served those rows "+
				"to NOBODY, which is an omission rather than a ceiling.\n"+
				"  THE PER-ROUND LINES ABOVE SAY WHICH PARTY THIS NAMES, and the two are not the same\n"+
				"  finding: a reader that reached epoch %d and then answered NOTHING on a further round\n"+
				"  trip is the omission; a reader that never reached epoch %d is a convergence failure.",
			line, len(aRounds), openedEpoch, openedEpoch, openedEpoch)
	}
	// AND THE VICTIM'S PRE-REMOVAL HALF IS STILL READABLE at the survivor that learned the removal
	// by INGEST rather than by making it.
	heldAtSurvivor := textsPresent(aGroup.Messages())
	check(heldAtSurvivor[cAtTwo],
		"the returning A's log has lost C's epoch-2 line. Item 241 keeps the history of a membership "+
			"change for the members who were there, and a removal is a membership change")
	// THE ROSTERS AGREE, ROW BY ROW, between the party that committed the removal and the party that
	// slept through it -- which is the whole content of "converges on the roster".
	parties = []*namedGroup{{"A", aGroup}, {"B", bGroup}}
	rostersAgree("the offline survivor came back and converged", parties, identities, openedEpoch,
		map[string]string{"A": "member", "B": "owner"}, 2)
	check(len(leavesOf(aGroup, cId, "the returning A")) == 0,
		"the returning A still reads C's identity at leaves %v", leavesOf(aGroup, cId, "the returning A"))
	sameRoster("B", bGroup, "the returning A", aGroup)
	// AND THEY SHARE THE NEW EPOCH'S KEY SCHEDULE, which agreeing on a number is not: opening each
	// other's line at the epoch the removal opened needs one storage_root, and the removed identity
	// holds no pq_secret for it.
	const afterTheRemoval = "typed by the survivor that was offline across the removal, at the epoch it opened"
	if _, err := aGroup.Send(ctx, afterTheRemoval); err != nil {
		fail("the returning A's Send at the epoch the removal opened: %v", err)
	}
	opens(bGroup, ctx, "B", "the returning A", afterTheRemoval)

	// ── STAGE 12: THE PER-PARTY LINE ──────────────────────────────────────────────────────────
	fmt.Fprintf(out, "  removal, per party:\n")
	removalReport("A  survivor, offline across it", aGroup, cId)
	removalReport("B  owner, committed it", bGroup, cId)
	removalReport("C  removed, two leaves", cGroup, cId)

	// ── the counters, which are the last thing a reader should see ───────────────────────
	step("the counters")
	report("A", aGroup)
	report("B", bGroup)
	report("C", cGroup)

	// ── the log this run leaves behind, read back by the run ─────────────────────────────
	step("this run's own output, read back for credentials")
	fmt.Fprintf(out, "  WHAT THIS IS FOR: the three things this binary is given are bearer credentials\n"+
		"  for real network clients on a live operator, and this output gets redirected into files,\n"+
		"  pasted into tickets and left in scrollback. No reading of the source can promise that no\n"+
		"  formatted error above carried one, because the errors come from four packages this file\n"+
		"  does not own. So every octet written was kept and is scanned here. The needle is the\n"+
		"  three-octet prefix every JWT this system mints begins with, and it is deliberately not\n"+
		"  printed: a success line that named it would put a hit in the very log an operator greps.\n")
	check(strings.Count(credentialControl, credentialNeedle) == 1,
		"CONTROL FAILED: the scanner finds %d occurrence(s) of the JWT prefix in a fabricated value "+
			"that carries exactly one by construction, so the count below is a property of a blind "+
			"instrument and not of this run's output",
		strings.Count(credentialControl, credentialNeedle))
	check(0 < transcript.octets(),
		"CONTROL FAILED: the transcript holds %d octets, so a count of zero is about nothing that was written",
		transcript.octets())
	// THE SAME SCANNER THE FAILURE PATH USES, and that is the one thing this block must not have its
	// own copy of: every failure below step 1 ends in os.Exit before this statement, so a scanner
	// written here and nowhere else would run only over runs in which nothing failed.
	inTheLog := scanForCredentials("this run's output")
	check(inTheLog == 0,
		"THIS RUN'S OWN OUTPUT CARRIES %d OCCURRENCE(S) OF THE THREE-OCTET PREFIX EVERY JWT BEGINS "+
			"WITH, and the disclosure sentence above says what to do about it", inTheLog)
	fmt.Fprintf(out, "  %d octets written, and none of them begins a JWT; the control finds its one\n",
		transcript.octets())
	fmt.Fprintf(out, "  the same scanner runs inside the failure path, so a run that FAILS is scanned over\n"+
		"  its own FAIL line rather than exiting before the check\n")

	fmt.Fprintf(out, "\n=== %d STEPS, %d ASSERTIONS, ALL HELD ===\n", steps, checks)
	fmt.Fprintf(out, "WHAT THIS PROBE DOES NOT ASSERT, and it needs a database credential this binary must\n"+
		"not hold: that the plaintext is absent from the server's `message_record` rows.\n")
}

// ── the parties ──────────────────────────────────────────────────────────────────────────────

// dial stands up one whole client: a connect client on the real mesh, a 10.1 transport over it,
// the DURABLE stream store the reserver allocates out of, and the DURABLE mls state store that
// makes a restart a restore.
//
// BOTH STORES LIVE UNDER ONE DIRECTORY PER PARTY and that directory is the whole of what survives
// this process. Nothing else about a party is persisted and nothing else needs to be.
//
// THE CLIENT IS sdk.NewMessageClient'S AND IT USED TO BE THIS FUNCTION'S. The eight lines that
// stood one up -- a client strategy, an out-of-band control over https://api.<host>, a client at
// the credential's client_id, a platform transport dialling wss://connect.<host>, and the provide
// modes -- were the ONLY construction of a platform-attached client anywhere in this workspace,
// which is why the C abi could reach nothing but an in-process loopback server. They are now
// sdk/message_client.go's, and this probe calls it.
//
// THAT IS THE POINT RATHER THAN A TIDY-UP. This binary is the one thing that is ever run against a
// real operator, so putting the shared declaration on ITS path is what makes a live run evidence
// about the code the Windows app links rather than about a copy of it.
// routeClient builds the client a transport speaks over: the operator-attached one, or a route
// to the server's own endpoint through a URnetwork exit or directly (sdk/message_route.go).
// The credential goes to the constructor and nowhere else.
func routeClient(ctx context.Context, route string, byJwt string, host string, endpoint string, pin string, appVersion string) (sdk.MessageTransportClient, interface{ Close() }, string) {
	if route == "" || route == "platform" {
		client, err := sdk.NewMessageClient(ctx, &sdk.MessageClientConfig{
			ByClientJwt: byJwt,
			Host:        host,
			AppVersion:  appVersion,
		})
		if err != nil {
			fail("client: %v", err)
		}
		return client, client, fmt.Sprintf("platform: client_id %s dialling %s", client.ClientId(), client.PlatformUrl())
	}
	mode, err := sdk.ParseMessageRouteMode(route)
	if err != nil {
		fail("-route: %v", err)
	}
	parsedPin, err := sdk.ParseMessageRoutePin(pin)
	if err != nil {
		fail("-pin: %v", err)
	}
	client, err := sdk.NewMessageRouteClient(ctx, &sdk.MessageRouteConfig{
		Endpoint:    endpoint,
		Pin:         parsedPin,
		Mode:        mode,
		ByClientJwt: byJwt,
		Host:        host,
		AppVersion:  appVersion,
	})
	if err != nil {
		fail("route: %v", err)
	}
	return client, client, fmt.Sprintf("route %s to %s", mode, endpoint)
}

// routeStatus prints what a route client knows about its route; nothing for the platform path.
func routeStatus(client sdk.MessageTransportClient) string {
	route, ok := client.(*sdk.MessageRouteClient)
	if !ok {
		return ""
	}
	status := route.Status()
	return fmt.Sprintf("route %s: connected=%v connects=%d window_providers=%d countries=%v last_error=%q",
		status.Mode, status.Connected, status.Connects, status.WindowProviders, status.WindowCountries, status.LastError)
}

func (self *dialer) dial(name string, jwtPath string) *party {
	raw, err := os.ReadFile(jwtPath)
	if err != nil {
		fail("%s read jwt: %v", name, err)
	}
	byJwt := strings.TrimSpace(string(raw))
	client, closer, described := routeClient(self.ctx, self.route, byJwt, self.host, self.endpoint, self.pin, "alphaprobe")

	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client: client, Server: self.server, ProtocolVersion: 1, Timeout: self.timeout,
	})
	if err != nil {
		fail("%s transport: %v", name, err)
	}
	dir := self.root + "/" + name
	if err := os.MkdirAll(dir+"/stream", 0o700); err != nil {
		fail("%s stream dir: %v", name, err)
	}
	if err := os.MkdirAll(dir+"/state", 0o700); err != nil {
		fail("%s state dir: %v", name, err)
	}
	streamStore, err := sdk.OpenStreamStore(dir + "/stream")
	if err != nil {
		fail("%s OpenStreamStore: %v", name, err)
	}
	stateStore, err := urmessage.OpenDurableStateStore(dir + "/state")
	if err != nil {
		fail("%s OpenDurableStateStore: %v -- if this says the directory is held, a previous run of this probe is still alive",
			name, err)
	}
	device, err := urmessage.NewDevice(urmessage.DeviceConfig{
		Transport:  transport,
		Reserver:   sdk.NewStreamIndexReserver(streamStore),
		StateStore: stateStore,
		Connect:    self.connect,
	})
	if err != nil {
		fail("%s NewDevice: %v", name, err)
	}
	// ── THE WRAP SEED, BEFORE ANY STEP SPENDS A ROUND TRIP ─────────────────────────────────────
	//
	// A STATE DIRECTORY WRITTEN BEFORE THE X-WING SEED EXISTED IS ANSWERED WITH A NIL ERROR AND AN
	// EMPTY SEED, by design and for a reason urmessage's DurableStateStore.GetDeviceIdentity states:
	// its one version lever is read for every record in the directory, so spending it on the
	// identity would refuse the group states and key packages beside it, and such a device could
	// never start again at all. What that leaves is a device that runs, sends and reads, and that
	// cannot open an encapsulation addressed to its own leaf -- so it can follow NO epoch anybody
	// else opens, because every epoch's pq_secret reaches it as a wrap to that leaf. The deployed
	// alpha's own directories are three-part ones: ledger item 257, whose ruling 53 is to re-found
	// that deployment rather than mint a replacement key.
	//
	// WITHOUT THIS LINE THE SYMPTOM ARRIVES LATE AND UNDER THE WRONG NAME. [urmessage.ErrNoDeviceWrapKey]
	// is folded into the group's unreadable-wrap counter at the one call site that opens a wrap, so
	// what a run sees is `a wrap that did not open` at step 5 or step 10 -- loud, but a sentence
	// about a ciphertext where the truth is a sentence about this directory. Item 257 recorded that
	// nothing in cgo or liveprobe surfaces a missing seed; this is liveprobe's half of it.
	//
	// AND IT IS HERE AND NOT IN A STEP because of what it would do to step 11. A device that derives
	// no epoch at all SATISFIES "the removed member cannot derive the epoch its own removal opened":
	// the clause would pass vacuously, on the one party it is about.
	//
	// THE DISCRIMINATOR IS THE SENTINEL AND NOT THE FAILURE.
	// [urmessage.Device.DecapsulateToOwnLeaf] answers ErrNoDeviceWrapKey before it looks at the
	// ciphertext when the seed is absent, and messagegroup's bad-ciphertext-size refusal when it is
	// present, so an EMPTY ciphertext separates the two with no key material anywhere near the
	// question. Step 11 carries the control that the yes arm is reachable at all.
	if _, err := device.DecapsulateToOwnLeaf(nil); errors.Is(err, urmessage.ErrNoDeviceWrapKey) {
		fail("%s's state directory %s HOLDS NO X-WING WRAP SEED.\n"+
			"  It was written before the seed existed -- a three part identity record -- so this device\n"+
			"  publishes a leaf whose private half nothing on this machine can reconstruct, and it can\n"+
			"  never open the wrap that carries a new epoch's pq_secret to that leaf. It would follow no\n"+
			"  epoch any other party opens, and step 11's `the removed member cannot derive the epoch its\n"+
			"  own removal opened` would PASS VACUOUSLY against it.\n"+
			"  MINTING A REPLACEMENT WOULD BE WORSE THAN THE ABSENCE: every ratchet tree already carries\n"+
			"  the old public half. RE-FOUND this deployment (ledger item 257, ruling 53), or point -dir\n"+
			"  at a fresh directory, which is what the README asks for anyway: %v", name, dir, err)
	}
	fmt.Fprintf(out, "%s  %s, durable state in %s (wrap seed present)\n",
		name, described, dir)
	return &party{
		name: name, dir: dir, client: closer, transport: transport,
		streamStore: streamStore, stateStore: stateStore, device: device,
	}
}

// restart kills a party and opens a new one over the same directory, the way the operating system
// would if the user closed the app.
//
// It re-reads the credential from the SAME flag the first dial used rather than carrying anything
// forward: a restart handed the parsed credential in memory would be carrying state across the
// thing it is meant to be measuring.
func (self *dialer) restart(previous *party) *party {
	self.kill(previous)
	// the exclusions are the operating system's and are released by the closes above; if the new
	// open is refused as locked, the previous process did NOT let go and that is the finding.
	return self.dial(previous.name, self.jwtOf(previous.name))
}

// kill drops everything a party holds in memory and touches nothing on its disk, which is what the
// operating system does when the user closes the app.
//
// IT IS SPLIT OUT OF [dialer.restart] BECAUSE ONE STEP NEEDS THE GAP. Step 7 kills a party and
// opens it again immediately; step 11 takes a survivor OFFLINE, drives a removal while it is gone,
// and only then brings it back -- and "offline across the removal" is exactly the interval between
// the two halves of a restart.
func (self *dialer) kill(previous *party) {
	fmt.Fprintf(out, "  killing %s: device, both durable stores, transport and connect client\n",
		previous.name)
	previous.close()
}

// stranger is a device with a FRESH identity that never connects: its one job is a key package
// naming an identity the group has never seen, which is what a member's refused add (step 10) has
// to carry -- a key package from any existing member would be that member's own second device,
// which a member of any role MAY add (MASTER 11's self-service rule). It rides the transport of
// the party it is built beside because a Device needs one, and it never speaks over it; its state
// store is the in-memory one, and its stream store is a directory of its own under -dir so that
// nothing of the real party's is touched.
func (self *dialer) stranger(beside *party) *party {
	dir := self.root + "/stranger"
	if err := os.MkdirAll(dir+"/stream", 0o700); err != nil {
		fail("the stranger's stream dir: %v", err)
	}
	streamStore, err := sdk.OpenStreamStore(dir + "/stream")
	if err != nil {
		fail("the stranger's OpenStreamStore: %v", err)
	}
	device, err := urmessage.NewDevice(urmessage.DeviceConfig{
		Transport: beside.transport,
		Reserver:  sdk.NewStreamIndexReserver(streamStore),
	})
	if err != nil {
		fail("the stranger's NewDevice: %v", err)
	}
	return &party{name: "stranger", dir: dir, streamStore: streamStore, device: device}
}

// jwtOf is where a party's credential path lives across a restart. The flags are the source of
// truth and this reads them back rather than keeping a copy.
func (self *dialer) jwtOf(name string) string {
	switch name {
	case "A":
		return flag.Lookup("a").Value.String()
	case "B":
		return flag.Lookup("b").Value.String()
	case "C":
		return flag.Lookup("c").Value.String()
	}
	fail("no credential flag for party %q", name)
	return ""
}

func (self *party) close() {
	if self.device != nil {
		self.device.Close()
	}
	if self.stateStore != nil {
		self.stateStore.Close()
	}
	if self.streamStore != nil {
		self.streamStore.Close()
	}
	if self.transport != nil {
		self.transport.Close()
	}
	if self.client != nil {
		self.client.Close()
	}
}

// ── the harness ──────────────────────────────────────────────────────────────────────────────

// printConnectAttempt is what makes "reconnecting" a thing an operator SEES rather than a thing
// the error says afterwards. A blocking Connect that prints nothing for ninety seconds is
// indistinguishable from a hang, which is most of why the old hard failure was tolerable.
func printConnectAttempt(attempt urmessage.ConnectAttempt) {
	fmt.Fprintf(out, "  reconnecting: Hello attempt %d was not answered after %v; waiting %v (%v)\n",
		attempt.Attempt, attempt.Elapsed.Round(time.Millisecond),
		attempt.Backoff.Round(time.Millisecond), attempt.Err)
}

// connectAcrossTheReconnectWindow is Device.Connect with the ONE failure that is the operator's
// named as the operator's, and not as this build's.
//
// IT IS A FUNCTION BECAUSE TWO STEPS RE-DIAL A client_id AND A SECOND COPY OF THE SENTENCE IS ONE
// THAT SAYS THE OLDER THING. Step 7 restarts B; step 11 takes a survivor offline across a removal
// and brings it back. Both land in the ~60 second window on every run, because both re-dial under a
// client_id that has just gone away. `what` names the party as a reader should see it and `untested`
// names what is still unmeasured if the budget runs out first -- which is the whole content of the
// distinction: a probe that called the window a failure of the thing it was about to test would be
// reporting the operator's item 5 as a defect in the restore, or in the convergence.
func connectAcrossTheReconnectWindow(ctx context.Context, p *party, what string, untested string) {
	began := time.Now()
	if err := p.device.Connect(ctx); err != nil {
		if errors.Is(err, urmessage.ErrReconnecting) {
			fail("%s was still not routed to after %v of retrying: %v\n"+
				"  THIS IS THE OPERATOR WINDOW AND NOT A FAILURE OF %s (operator item 5).\n"+
				"  Raise -reconnect above the window and run again; %s is untested until this\n"+
				"  call returns.", what, time.Since(began).Round(time.Second), err, untested, untested)
		}
		fail("%s Connect: %v", what, err)
	}
	fmt.Fprintf(out, "  %s was routed to after %v\n", what, time.Since(began).Round(time.Millisecond))
}

func step(s string) {
	steps += 1
	current = s
	fmt.Fprintf(out, "\n=== %d. %s ===\n", steps, s)
}

// check is the whole of the probe's discipline: every assertion carries the sentence that says
// what was expected and what came back, so a failure at 2am is readable without this source.
func check(held bool, format string, args ...any) {
	checks += 1
	if !held {
		fail(format, args...)
	}
}

// receive is [urmessage.Group.Receive] with its error treated as fatal AND its truncation signal
// treated as fatal, which is the point of the signal existing.
func receive(group *urmessage.Group, ctx context.Context, who string) []*urmessage.Message {
	got, err := group.Receive(ctx)
	if err != nil {
		if errors.Is(err, urmessage.ErrFetchIncomplete) {
			fail("%s Receive stopped at its page bound with %d messages read; the server still has more: %v",
				who, len(got), err)
		}
		if errors.Is(err, urmessage.ErrIdentityInUse) {
			fail("%s: ANOTHER DEVICE IS SEALING UNDER THIS DEVICE'S IDENTITY IN THIS GROUP. That is a COPY\n"+
				"  of the app-data directory -- two devices at one leaf, one sender_handle and one stream\n"+
				"  counter -- and two records under one (epoch, sender_handle, stream_index) are one\n"+
				"  record_key and one nonce, which 5.6 calls a total break of both AEADs for that record.\n"+
				"  One of the two copies has to stop -- this is not a probe artefact and not something\n"+
				"  re-running fixes. NOTE it is NOT what running this probe twice over one -dir does:\n"+
				"  a second run founds a fresh group id, so the old group's indices still match its own\n"+
				"  reserver. This means a COPY of the directory exists somewhere: %v", who, err)
		}
		if errors.Is(err, urmessage.ErrFetchOmitted) {
			fail("%s: THE SERVER ANSWERED A COMPLETE PAGE AND NAMED A HIGH WATER ABOVE EVERYTHING IT HANDED\n"+
				"  OVER. It is holding records back, which is the one failure the AEAD cannot see.\n"+
				"  THE ONE INNOCENT EXPLANATION IS NOT BUILT YET: 7.2's retention sweep would take rows\n"+
				"  out from under a high water that is next_record_id-1 and does not come down, and\n"+
				"  nothing in the server deletes a message_record row today. If you are running against a\n"+
				"  server new enough to sweep, check its retention settings before reading this as an\n"+
				"  omission; otherwise read it as one: %v", who, err)
		}
		if errors.Is(err, urmessage.ErrRecordAbandoned) {
			fail("%s: a record did not open after every retry and is no longer being fetched, so this\n"+
				"  conversation has a hole in it. Records given up on: %v -- %v",
				who, group.UnopenedRecords(), err)
		}
		fail("%s Receive: %v", who, err)
	}
	return got
}

// receiveRound is one round of [receiveAcrossTheEpochCeiling]: the epoch it left the group at.
//
// ONE FIELD, BECAUSE ONE FIELD IS WHAT ANYTHING ASKS ABOUT. The FIRST round's epoch is where the
// one-epoch-per-round-trip rule is held -- it is the assertion stage 11 made when it believed a
// single Receive was the whole stage -- and `len(rounds)` is how many round trips it took. The
// UNION of the rounds is where the content assertions are held, because content sealed above the
// ceiling is not in the round that crosses it.
//
// AND IT USED TO CARRY AN `entries` COUNT THAT NOTHING READ, under a header saying both fields were
// asserted on. There is no assertion for it to hold that is true either way item 246's ceiling is
// deployed: with the ceiling the crossing round is short of the rounds above it, without it the
// first round is the whole answer, and a probe that has to pass in both cannot pin either shape.
// What the counts are FOR is a human reading a red, and the per-round line below prints them.
type receiveRound struct {
	epoch uint64
}

// receiveAcrossTheEpochCeiling is [receive] REPEATED until the group answers nothing and crosses
// nothing, with every round's entries unioned into one answer.
//
// WHY ONE Receive IS THE WRONG UNIT FOR A PARTY THAT MUST CROSS AN EPOCH *AND* READ ABOVE IT. Under
// ledger item 246's F0 ceiling the store serves only rows whose epoch is at or below the `read_epoch`
// inside the request's own req_auth, and the client sends `ReadEpoch: self.epoch` fresh on each page
// (urmessage/group.go:3225). A reader one commit behind is therefore served that commit and NOTHING
// ABOVE IT -- and the page it gets back is COMPLETE, because the ceiling is a FILTER and not a
// truncation: msgrepo's `store.MemoryStore.Fetch` opens with `Complete: true` and `continue`s over
// every row above the ceiling, and its own comment says the filter is deliberate. A complete page
// ends the walk at urmessage/group.go:3278, so the epoch the ingest just moved is only ever used by
// a page the walk no longer asks for. ONE Receive crosses ONE EPOCH, by design. msgrepo holds that
// end to end in TestAMemberSeveralEpochsBehindWalksForwardOneEpochPerRoundTrip, and cp3b's
// `rolesReceiveAll` drains for exactly this reason and says so.
//
// SO THIS IS NOT A RETRY LOOP AND IT IS NOT [receive] WITH SLACK ADDED. What one Receive produces for
// such a party is not an error at all: it is a party at the right epoch holding an empty answer --
// which is indistinguishable from a server that served those rows to NOBODY. Telling those two apart
// is stage 11's whole job as stage 9's control, so a stage built on one Receive would have failed
// naming the wrong party.
//
// EXACTLY ONE CALL SITE IN THIS FILE DRAINS, and that is deliberate rather than conservative. [opens]
// and [countOnce] assert EXACT entry counts on groups that are already at the head, where a blanket
// drain would break the counts they hold; the party this is for is the one party in the run that
// slept through a commit.
//
// THE BOUND IS A FAILURE AND NOT A break. A group that will not settle is a real defect, and a drain
// that gave up quietly would hand every assertion below it a partial union and call it converged.
func receiveAcrossTheEpochCeiling(group *urmessage.Group, ctx context.Context,
	who string) ([]*urmessage.Message, []receiveRound) {

	// THE ONE CALL SITE CROSSES ONE EPOCH AND SETTLES IN THREE ROUNDS: one that crosses it carrying
	// the history below, one that reads what was sealed above it, one that answers nothing at an
	// epoch it did not move. Eight is five rounds of slack and still a number a stall cannot hide in.
	const maxRounds = 8
	union := []*urmessage.Message{}
	rounds := []receiveRound{}
	for at := 1; at <= maxRounds; at += 1 {
		before := group.Epoch()
		got := receive(group, ctx, who)
		union = append(union, got...)
		rounds = append(rounds, receiveRound{epoch: group.Epoch()})
		fmt.Fprintf(out, "  %s's round trip %d across the ceiling: %d entr%s, epoch %d -> %d\n",
			who, at, len(got), map[bool]string{true: "y", false: "ies"}[len(got) == 1],
			before, group.Epoch())
		// nothing new and no epoch crossed: this reader is at its own ceiling's head, which under F0
		// is the only "caught up" a reader can observe about itself.
		//
		// THE EPOCH CLAUSE IS A GUARD OVER THE MECHANISM AND NOT OVER THIS ONE CALL SITE, and saying
		// which is the correction of a published measurement. Its subject is a reader whose cursor is
		// already past its history: under the ceiling such a reader is served the one commit above it
		// and NOTHING above that on a page that comes back COMPLETE, so its crossing round answers
		// zero entries while the epoch moves, and "nothing new" alone would call that caught up with
		// content still unread. MEASURED in cp3b against the real server's own api.Handler and
		// store.MemoryStore, in that shape: rounds [{0 3} {3 3} {0 3}] with the clause, and with the
		// clause dropped ONE round [{0 3}] missing 3 of the 3 lines sealed above the ceiling.
		//
		// AT THIS FILE'S ONE CALL SITE IT IS NOT LOAD-BEARING, and that is stated rather than left to
		// be rediscovered. The returning A comes back through a RESTART and the receive cursor is not
		// persisted, so its crossing round re-walks its whole history and answers MANY entries, not
		// zero. Measured in the same harness, same server, in the call site's shape: rounds
		// [{6 3} {3 3} {0 3}] with the clause and the SAME [{6 3} {3 3} {0 3}] without it, missing
		// nothing either way. So the mutant does not convict here; it convicts the day the cursor is
		// persisted, or the day a second call site drains a party that never died.
		if len(got) == 0 && group.Epoch() == before {
			return union, rounds
		}
	}
	fail("%s did not settle after %d Receive round(s), stalled at epoch %d having read %d entr%s.\n"+
		"  Under item 246's ceiling a reader walks forward ONE EPOCH PER ROUND TRIP, so this loop ends\n"+
		"  when a round answers nothing AND crosses nothing. A group that never reaches that is not a\n"+
		"  slow drain: it is a reader whose epoch or cursor keeps moving without converging, and the\n"+
		"  per-round lines above say which of the two it is.",
		who, maxRounds, group.Epoch(), len(union),
		map[bool]string{true: "y", false: "ies"}[len(union) == 1])
	return nil, nil
}

// opens is one direction of a group chat: [receive] on the far side, then the assertion that the
// exact text came back OPENED -- a line and not a gap -- and that it came back ALONE, because
// every group here has drained before the send, so a second entry is a record nobody sent.
func opens(group *urmessage.Group, ctx context.Context, reader string, writer string, want string) {
	got := receive(group, ctx, reader)
	check(len(got) == 1, "%s read %d entries for the one line %s sent: %s", reader, len(got), writer, texts(got))
	check(got[0].Gap == "", "%s received %s's line as a %q gap rather than opening it", reader, writer, got[0].Gap)
	check(got[0].Text == want, "%s opened %q and %s typed %q", reader, got[0].Text, writer, want)
	check(!got[0].Mine, "%s opened %s's line and its own log marks it as %s's own", reader, writer, reader)
	fmt.Fprintf(out, "  %s->%s opened on %s: %q\n", writer, reader, reader, got[0].Text)
}

// gapCount sorts one Receive's entries three ways: out_of_window gaps, opened lines, and gaps of
// any OTHER reason -- which the epoch change never produces, so the third is asserted zero.
func gapCount(got []*urmessage.Message) (outOfWindow int, opened int, other int) {
	for _, one := range got {
		switch one.Gap {
		case "":
			opened += 1
		case urmessage.GapOutOfWindow:
			outOfWindow += 1
		default:
			other += 1
		}
	}
	return outOfWindow, opened, other
}

// countOnce holds that every expected line arrived EXACTLY once, which is what a concurrent send
// can break in both directions: a lost line and a duplicated one.
func countOnce(got []*urmessage.Message, expected map[string]bool, reader string, writer string) {
	seen := map[string]int{}
	for _, one := range got {
		seen[one.Text] += 1
	}
	for line := range expected {
		check(seen[line] == 1, "%s read %s's line %q %d times, want exactly 1", reader, writer, line, seen[line])
	}
	check(len(got) == len(expected), "%s read %d messages and %s sent %d", reader, len(got), writer, len(expected))
}

// namedGroup is one party's group under the name the tables print.
type namedGroup struct {
	name  string
	group *urmessage.Group
}

// identityOf is a party's own identity public key, read off its own roster's Mine row -- the
// value SetRole and TransferOwnership take, and the key every roster is joined on.
func identityOf(group *urmessage.Group, who string) []byte {
	members, err := group.Members()
	if err != nil {
		fail("%s Members: %v", who, err)
	}
	for _, member := range members {
		if member.Mine {
			return append([]byte(nil), member.IdentityPub...)
		}
	}
	fail("%s's roster marks no row as its own", who)
	return nil
}

// rolesAgree is [rostersAgree] where every identity holds exactly one leaf, which is every stage of
// step 10 and the shape this function had when step 10 was the last one. It is not a second decision:
// one row per identity IS `len(want)` rows.
func rolesAgree(stage string, parties []*namedGroup, identities map[string]string, epoch uint64, want map[string]string) {
	rostersAgree(stage, parties, identities, epoch, want, len(want))
}

// rostersAgree prints one roles table per party -- every row of its OWN Members(), with the epoch
// -- and asserts four things at once: every party is at the wanted epoch, every party's roster holds
// exactly `rows` rows, every wanted identity holds exactly the wanted role, and no party's roster
// names anybody else. A disagreement is a FAIL line naming the party, the identity and both roles.
//
// ROWS AND IDENTITIES ARE TWO NUMBERS AND STEP 11 IS WHERE THEY COME APART. One identity may hold
// several device leaves and then appears once PER LEAF with the same role on each, so a roster of
// three members can be four rows -- and a version of this function that counted rows as identities
// would call C's own second device a fourth party.
func rostersAgree(stage string, parties []*namedGroup, identities map[string]string, epoch uint64,
	want map[string]string, rows int) {

	fmt.Fprintf(out, "  roles after %q:\n", stage)
	for _, one := range parties {
		members, err := one.group.Members()
		if err != nil {
			fail("%s Members after %s: %v", one.name, stage, err)
		}
		row := []string{}
		seen := map[string]string{}
		for _, member := range members {
			name, known := identities[string(member.IdentityPub)]
			if !known {
				name = "?" + fmt.Sprintf("%x", member.IdentityPub[:4])
			}
			mine := ""
			if member.Mine {
				mine = "*"
			}
			row = append(row, fmt.Sprintf("leaf%d %s%s=%s", member.LeafIndex, name, mine, member.Role))
			seen[name] = member.Role
		}
		fmt.Fprintf(out, "    %s @epoch %d: %s\n", one.name, one.group.Epoch(), strings.Join(row, "  "))
		check(one.group.Epoch() == epoch, "%s is at epoch %d after %s, want %d", one.name, one.group.Epoch(), stage, epoch)
		check(len(members) == rows, "%s's roster holds %d rows after %s, want %d", one.name, len(members), stage, rows)
		for name, role := range want {
			check(seen[name] == role, "%s reads %s as %q after %s, want %q", one.name, name, seen[name], stage, role)
		}
		for name := range seen {
			if _, wanted := want[name]; !wanted {
				fail("%s's roster names %s, which no party is, after %s", one.name, name, stage)
			}
		}
	}
}

func report(who string, group *urmessage.Group) {
	stats := group.Stats()
	fmt.Fprintf(out, "  %s: fetched=%d opened=%d ceremony=%d own=%d otherClasses=%d FAILED=%d submitted=%d rebound=%d pages=%d unattested=%d\n",
		who, stats.Fetched, stats.Opened, stats.SkippedCeremony, stats.SkippedOwn,
		stats.SkippedClass, stats.FailedOpen, stats.Submitted, stats.Rebound, stats.Pages, stats.Unattested)
	check(stats.FailedOpen == 0, "%s: %d records from a member of this group did not open", who, stats.FailedOpen)
}

// ── step 11's own instruments ────────────────────────────────────────────────────────────────

// removalCeilingLines is how many lines the survivor seals ABOVE the epoch the removal opened, for
// item 246's ceiling. More than one, because a single row cannot tell a page bound from a filter;
// few, because each is a round trip.
const removalCeilingLines = 3

// assertRemoved holds RULING 52's whole predicate over one answer: it IS the removal, it carries
// mls's own cause, and it is none of the states the ruling says it must be distinguishable from.
//
// IT IS A FUNCTION BECAUSE THE PREDICATE IS THE POINT AND STEP 11 ASKS IT NINE TIMES. A clause
// spelled nine times is eight chances for one of them to be the weaker spelling -- and the weaker
// spelling here is `err != nil`, which every one of the states below also satisfies.
//
// IT IS cp3b's removalAssertRemoved OVER A REAL SERVER. That helper drives the same predicate in one
// process; what this one adds is the transport, whose own failures --
// [urmessage.ErrFetchRefused] and the not-reconciled refusal a RESTORED group answers until its
// first clean walk -- are in the list precisely because they are the two a live run could get
// instead and a loopback run cannot.
func assertRemoved(what string, err error) {
	check(errors.Is(err, urmessage.ErrRemovedFromGroup),
		"%s answered %v, want urmessage.ErrRemovedFromGroup. A removed device that is answered a nil "+
			"error, or a generic one, reads as caught up and silent -- which is the state ruling 52 "+
			"exists to end", what, err)
	check(errors.Is(err, mls.ErrRemovedFromGroup),
		"%s does not carry mls.ErrRemovedFromGroup, which is the cause: %v", what, err)
	// NAMED AND NOT GENERIC: ErrCommitIngest is what a bent ciphertext answers too, and a caller that
	// saw it here would read a membership that ended as a transient worth retrying.
	check(!errors.Is(err, urmessage.ErrCommitIngest),
		"%s also answers ErrCommitIngest, so a removal cannot be told from a commit that did not open: %v",
		what, err)
	for _, other := range []struct {
		name string
		err  error
	}{
		{"ErrRemovalWithoutRotation (ruling 41's halt: a commit this device REFUSED)", urmessage.ErrRemovalWithoutRotation},
		{"ErrNoWrapForEpoch (ruling 38: a commit it FOLLOWED with no keys)", urmessage.ErrNoWrapForEpoch},
		{"ErrWrapUnreadable", urmessage.ErrWrapUnreadable},
		{"ErrOrphanWrap", urmessage.ErrOrphanWrap},
		{"ErrRecordAbandoned (a record that did not open)", urmessage.ErrRecordAbandoned},
		{"ErrFetchRefused (the transport)", urmessage.ErrFetchRefused},
		{"ErrNotReconciled", urmessage.ErrNotReconciled},
		{"ErrStreamFloorUnheld", urmessage.ErrStreamFloorUnheld},
	} {
		check(!errors.Is(err, other.err),
			"%s also answers %s; ruling 52's whole content is that this state is distinguishable from "+
				"that one: %v", what, other.name, err)
	}
}

// secondLeafFor is a key package for a SECOND DEVICE LEAF of an identity that is already in the
// group: its own signature key, its own X-Wing key, and a credential naming the identity handed in.
//
// IT IS THE ONLY WAY THIS BINARY CAN PUT TWO LEAVES OF ONE IDENTITY IN A GROUP, and the reason is a
// decision and not a gap: a urmessage.Device's credential identity IS its signature key, and one
// Device mints one identity per state store, so that package has no door onto a second leaf of an
// existing identity. cp3b's world.seamMemberClaiming is the same construction one module over, and
// cp3b's TestOneCallRemovesEveryLeafOfOneIdentityAndTheRemovedMemberCannotFollow is driven with it.
//
// IT IS NOT A FORGERY, AND WHAT MAKES IT NOT ONE IS WHO COMMITS THE ADD. MASTER §11's self-service
// rule gives every member its own device leaves (ruling 2, and ruling 5 for an OBSERVER), and R6a
// requires an Add claiming an identity already in the group to be committed BY that identity -- so
// this key package is a second device of that member when that member adds it, and is refused at
// every honest receiver when anybody else does.
//
// IT HOLDS NO TRANSPORT AND NEVER READS. The engine is built over an in-memory state store because
// nothing restores this leaf: what the step needs from it is that it OCCUPIES A LEAF, which is what
// a removal vector is about. A leaf that also fetched would need a fourth credential.
func secondLeafFor(what string, identity []byte) []byte {
	crypto, err := mls.NewCryptoProvider(mls.CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		fail("%s: the mls crypto provider: %v", what, err)
	}
	signer, _, err := crypto.SignatureKeyPair()
	if err != nil {
		fail("%s: the signature key pair: %v", what, err)
	}
	xwing, err := messagegroup.XwingGenerateKey(rand.Reader)
	if err != nil {
		fail("%s: the x-wing key: %v", what, err)
	}
	leafKeys, err := (&mls.LeafKeysExtension{AlgId: mls.AlgIdXwing, DeviceXwingPub: xwing.Public().Bytes()}).Encode()
	if err != nil {
		fail("%s: the leaf keys extension: %v", what, err)
	}
	engine, err := messagegroup.NewConnectMlsEngine(crypto, urmessage.NewMemoryStateStore(), signer,
		mls.BasicCredential(identity), leafKeys.ExtensionData)
	if err != nil {
		fail("%s: the engine: %v", what, err)
	}
	keyPackage, err := engine.NewKeyPackage()
	if err != nil {
		fail("%s: the key package: %v", what, err)
	}
	return keyPackage
}

// leavesOf is every leaf one identity holds in a group's roster, in leaf order. It is the reading
// "one call removes EVERY leaf of an identity" is asserted with, at both ends: two before, none
// after, and one each for the survivors in the same loop.
func leavesOf(group *urmessage.Group, identity []byte, who string) []uint32 {
	members, err := group.Members()
	if err != nil {
		fail("%s Members: %v", who, err)
	}
	leaves := []uint32{}
	for _, member := range members {
		if bytes.Equal(member.IdentityPub, identity) {
			leaves = append(leaves, member.LeafIndex)
		}
	}
	return leaves
}

// textsPresent is the set of texts a slice of entries carries. A GAP CONTRIBUTES NOTHING: it has no
// text, and letting it contribute "" would put the empty string in the set and make a lookup for a
// line nobody sent answer yes.
func textsPresent(messages []*urmessage.Message) map[string]bool {
	present := map[string]bool{}
	for _, one := range messages {
		if one.Gap != "" {
			continue
		}
		present[one.Text] = true
	}
	return present
}

// rosterRows is one party's roster as comparable strings, in the leaf order Members() answers: the
// leaf, the head of the identity and the role.
func rosterRows(group *urmessage.Group, who string) []string {
	members, err := group.Members()
	if err != nil {
		fail("%s Members: %v", who, err)
	}
	rows := []string{}
	for _, member := range members {
		rows = append(rows, fmt.Sprintf("leaf%d/%x/%s", member.LeafIndex, member.IdentityPub[:4], member.Role))
	}
	return rows
}

// sameRoster holds that two parties read the SAME roster, row by row.
//
// IT IS NOT WHAT [rostersAgree] HOLDS, AND THE DIFFERENCE IS THE POINT OF HAVING BOTH. rostersAgree
// asks each party whether it reads what this step EXPECTS -- three names with three roles -- and two
// parties can both satisfy an expectation while disagreeing about a leaf index or about a fourth row
// the expectation does not mention. This asks the two rosters about each other, so "converges on the
// roster" is a statement about the parties rather than about the step's own table.
func sameRoster(leftName string, left *urmessage.Group, rightName string, right *urmessage.Group) {
	leftRows, rightRows := rosterRows(left, leftName), rosterRows(right, rightName)
	check(len(leftRows) == len(rightRows), "%s reads %d roster row(s) and %s reads %d: %s against %s",
		leftName, len(leftRows), rightName, len(rightRows),
		strings.Join(leftRows, " "), strings.Join(rightRows, " "))
	for at := range leftRows {
		check(leftRows[at] == rightRows[at], "roster row %d is %q at %s and %q at %s",
			at, leftRows[at], leftName, rightRows[at], rightName)
	}
	fmt.Fprintf(out, "  %s and %s read the same %d roster row(s): %s\n",
		leftName, rightName, len(leftRows), strings.Join(leftRows, "  "))
}

// removalReport is step 11's per-party line, and it is [report]'s shape for a different question: not
// "what did this party see" but "where does this party stand on the removal".
//
// A REMOVED GROUP IS NOT ASKED FOR A ROSTER. mls closes the group and zeroizes its epoch secrets when
// it answers the removal, so Members() there answers a sentence about a data structure; what such a
// party has to say is the removal state, which it holds whatever mls has done. That is why the roster
// columns are a dash for it rather than a failure.
func removalReport(who string, group *urmessage.Group, victim []byte) {
	stats := group.Stats()
	removedAt, state := group.Removal()
	rows, role, standing := "-", "-", ""
	if state == nil {
		members, err := group.Members()
		if err != nil {
			fail("%s Members for the removal report: %v", who, err)
		}
		victimLeaves := 0
		for _, member := range members {
			if bytes.Equal(member.IdentityPub, victim) {
				victimLeaves += 1
			}
			if member.Mine {
				role = member.Role
			}
		}
		rows = fmt.Sprintf("%d", len(members))
		standing = fmt.Sprintf("a member; the removed identity holds %d leaf/leaves here", victimLeaves)
	} else {
		standing = fmt.Sprintf("REMOVED at epoch %d, and says so by name on every walk and every send", removedAt)
	}
	fmt.Fprintf(out, "    %-32s epoch=%-3d roster=%-3s role=%-8s %s\n",
		who, group.Epoch(), rows, role, standing)
	fmt.Fprintf(out, "    %-32s fetched=%d opened=%d ingested=%d refusedOwn=%d refused=%d wraps=%d "+
		"pastEpoch=%d FAILED=%d gaps=%d/%d\n", "",
		stats.Fetched, stats.Opened, stats.Ingested, stats.CommitRefusedOwn, stats.CommitRefused,
		stats.WrapOpened, stats.OpenedPastEpoch, stats.FailedOpen, stats.GapOutOfWindow, stats.GapMalformed)
	check(stats.FailedOpen == 0, "%s: %d record(s) from a member of this group did not open", who, stats.FailedOpen)
	check(stats.GapMalformed == 0,
		"%s: %d malformed gap(s). A record whose sender_handle resolves to no leaf of this group is what "+
			"a removal looks like to a party that did not file the departed leaf", who, stats.GapMalformed)
}

// texts renders entries for a failure message. THE LOCAL IS NOT NAMED `out` ANY MORE: `out` is the
// package-level writer every print in this file goes through, and a helper that shadowed it with a
// []string would make the next Fprintf written in here a compile error at best and a silent
// bypass of the credential scan at worst.
func texts(messages []*urmessage.Message) string {
	rendered := []string{}
	for _, one := range messages {
		if one.Gap != "" {
			// a gap has no text, and rendering it as "" would make it a blank line somebody sent
			rendered = append(rendered, fmt.Sprintf("<gap:%s@%d>", one.Gap, one.RecordId))
			continue
		}
		if 80 < len(one.Text) {
			rendered = append(rendered, fmt.Sprintf("%q...(%d octets)", one.Text[:80], len(one.Text)))
			continue
		}
		rendered = append(rendered, fmt.Sprintf("%q", one.Text))
	}
	return "[" + strings.Join(rendered, " ") + "]"
}

// firstDifference is where two strings stop agreeing, so a fragment reassembled out of order says
// WHERE rather than only that it is wrong.
// findById is the probe's one lookup: a message by the id its own sender was told it has.
//
// IT EXISTS BECAUSE NAMING IS THE WHOLE OF WHAT THE ENVELOPE ADDED. A reply that points at
// nothing, and a reaction that lands on nothing, both look like success from the sending side.
func findById(messages []*urmessage.Message, id []byte) *urmessage.Message {
	for _, message := range messages {
		if bytes.Equal(message.MessageId, id) {
			return message
		}
	}
	return nil
}

func firstDifference(want string, got string) int {
	for at := 0; at < len(want) && at < len(got); at += 1 {
		if want[at] != got[at] {
			return at
		}
	}
	if len(want) != len(got) {
		return min(len(want), len(got))
	}
	return -1
}

// scanForCredentials counts the JWT prefix in everything this run has written SO FAR and announces a
// disclosure if it finds any. It answers the count.
//
// IT HAS TWO CALLERS AND THAT IS THE WHOLE POINT OF IT BEING A FUNCTION. The final step scans because
// a clean run should say so; [fail] scans because THE PRINT MOST LIKELY TO CARRY A CREDENTIAL IS THE
// FAIL LINE ITSELF -- the errors formatted into it come from four packages this file does not own,
// which is the file's own stated reason for having the check at all. Every failure path here ends in
// os.Exit, so a scan that lived only in the final step would be a scanner the failing run never
// reaches: exactly the run whose log an operator is about to paste into a ticket.
//
// THAT WAS THE DEFECT THIS SHAPE REPAIRS, and it was measured on the built binary rather than read.
// Before it, `liveprobe -server not-a-valid-id ...` printed one FAIL line and the read-back's own
// success line never appeared at all -- the scan sat in the last statement of main, which os.Exit
// skips. Both arms of the repair were then driven: with no needle anywhere the failing run prints
// nothing extra, and with a FABRICATED JWT-shaped value in the flag the failing run prints the
// disclosure.
//
// IT DOES NOT CALL [fail] AND THAT IS NOT A STYLE CHOICE: [fail] is one of its two callers, so a
// scanner that failed would recurse for ever on the one run where it matters. The final step wraps
// the count in a [check] instead, which is where the assertion belongs.
//
// THE NEEDLE IS NOT IN WHAT IT PRINTS. A disclosure line that named the prefix would put a hit in
// the very log it is telling the operator to grep, and the next run's scan would find it.
func scanForCredentials(about string) int {
	held, announce := transcript.count(credentialNeedle)
	if announce {
		fmt.Fprintf(errOut, "\n"+
			"CREDENTIAL DISCLOSURE: %d occurrence(s) of the three-octet prefix every JWT this system\n"+
			"  mints begins with, in %s.\n"+
			"  A CREDENTIAL HAS REACHED THE LOG. Find them with `grep -c` for that prefix over this\n"+
			"  run's output, treat the credentials the three -a/-b/-c files hold as DISCLOSED, and mint\n"+
			"  replacements before anything else.\n", held, about)
	}
	return held
}

func fail(format string, args ...any) {
	if current != "" {
		fmt.Fprintf(errOut, "\nFAIL at step %d (%s)\n", steps, current)
	}
	fmt.Fprintf(errOut, "FAIL: "+format+"\n", args...)
	// THE SCAN RUNS ON THE FAILING PATH TOO, and it runs AFTER the print rather than over a
	// pre-formatted copy of it: `errOut` tees into the transcript, so by this line the FAIL text and
	// the step line above it are already octets the scan can see. That is why this costs one call and
	// no duplicated formatting -- and why deleting the tee on `errOut` would silently narrow it.
	scanForCredentials("this run's output, including the FAIL line above")
	os.Exit(1)
}
