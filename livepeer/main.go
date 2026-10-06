// THE SECOND PARTY. liveprobe drives BOTH sides of a conversation in one process; this drives ONE
// side, so that the other side can be a different process — the Windows app.
//
// WHY THIS EXISTS AT ALL. A solo device cannot open a group: Group.Open comes after AddMember, the
// alpha accepts exactly one AddMember, and one credential means no second key package. So an app
// holding one credential founds a group at epoch 0 that never opens, and every send is refused.
// THE MISSING HALF IS A SECOND PARTY, and this is it.
//
// THE SEAM IS TWO FILES AND IT IS A HANDOFF, not a protocol:
//
//	the app                                   this helper
//	───────                                   ───────────
//	device.KeyPackage() -> -keypackage   →    reads it, deletes it
//	                                          CreateGroup, AddMember(kp), Open
//	reads it, deletes it                 ←    invite.Encode() -> -invite
//	device.Join(invite)                       Send / SendReply / React
//
// EACH FILE IS CONSUMED BY ITS READER AND DELETED BY ITS READER, and that is not tidiness. A key
// package is SINGLE USE — the private halves are taken destructively at the join (device.go:510) —
// so a stale key package file read by a second run of this helper builds an invite that the app
// CANNOT join, and the failure arrives at the join rather than at the read. Deleting on consumption
// is what makes "the file is there" mean "the file is fresh".
//
// WHAT IS SECRET HERE. The credential is an operator-minted by_client_jwt and is read from a path;
// it is never printed. The INVITE is key material in full — an invite that reaches a third party is
// a group that third party is in — so it is written 0600 to a path outside any repository and
// deleted as soon as it is consumed. Neither is ever formatted into a log line.
//
// NEVER RUN THIS UNDER THE APP'S OWN ACCOUNT. Two clients at one client_id, or two state
// directories descended from one, is two devices at ONE MLS leaf: one sender_handle, one stream
// counter, and therefore a reused (epoch, sender_handle, stream_index) — a reused nonce under a
// reused record key, which the spec calls a total break of both AEADs. The app is user1; this is
// user2; each has its own -dir.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/sdk/v2026/urmessage"
)

type party struct {
	client      interface{ Close() }
	transport   *sdk.MessageTransport
	streamStore *sdk.StreamStore
	stateStore  *urmessage.DurableStateStore
	device      *urmessage.Device
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

func main() {
	jwtPath := flag.String("jwt", "", "file holding this peer's by_client_jwt — NOT the app's")
	serverId := flag.String("server", "01a0a199-5b06-5117-e86a-4bf5c01db15c", "the message server's client_id")
	host := flag.String("host", "beta-test.net", "operator host")
	dir := flag.String("dir", "", "this peer's OWN durable state directory; never the app's and never a copy of it")
	keyPackagePath := flag.String("keypackage", "", "file the app wrote its key package to; consumed and deleted")
	invitePath := flag.String("invite", "", "file this helper writes the invite to; KEY MATERIAL, 0600, outside any repo")
	joinCode := flag.String("joincode", "", "a base64 JOIN CODE pasted out of the app (or @FILE holding one); replaces -keypackage, and is what works between two machines")
	printInvite := flag.Bool("printinvite", false, "print the invite as a base64 INVITE CODE on stdout, for pasting back into the app")
	outbox := flag.String("outbox", "", "a file this peer WATCHES: every line appended to it is sent into the group, so somebody on this side can hold a conversation without restarting and paying the reconnect window again")
	quiet := flag.Bool("quiet", false, "skip the three scripted lines, the reply and the two reactions; restore, serve, and send only what -outbox gets")
	waitFor := flag.Duration("wait", 5*time.Minute, "how long to wait for the key package file to appear")
	serve := flag.Duration("serve", 4*time.Minute, "how long to keep fetching after the sends, so the app's own lines are seen")
	poll := flag.Duration("poll", 3*time.Second, "how often to fetch while serving")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request transport timeout")
	reconnect := flag.Duration("reconnect", 0, "how long Device.Connect rides out the ~60s reconnect window; 0 takes urmessage's own default")
	route := flag.String("route", "platform", "platform (the operator path), urnetwork (the server's own endpoint through a URnetwork exit) or direct (that endpoint, no tunnel)")
	endpoint := flag.String("endpoint", "", "the server's own endpoint, a wss:// url; required unless -route platform")
	pin := flag.String("pin", "", "SHA-256 of the endpoint's SubjectPublicKeyInfo, 64 hex characters; required unless -route platform")
	flag.Parse()

	// TWO ROADS IN AND TWO ROADS OUT, and a run needs one of each.
	//
	//   in:   -keypackage FILE   the developer road: the app writes it beside its credential
	//         -joincode  TEXT    the person road: they copied it out of the app and sent it
	//   out:  -invite FILE       the developer road: the app polls for it on the same disk
	//         -printinvite       the person road: paste the printed code back into their app
	//
	// The CODE roads are the ones that work when the two people are not at the same machine,
	// which is every test with a real second person in it.
	if *jwtPath == "" || *dir == "" {
		fail("-jwt and -dir are required.\n" +
			"  -jwt is this PEER's credential (user2). Handing it the app's (user1) is the one\n" +
			"  mistake this helper cannot detect and the one that breaks both AEADs.")
	}
	if *keyPackagePath == "" && *joinCode == "" {
		fail("one of -keypackage or -joincode is required: this helper cannot mint the app's key\n" +
			"  package, only consume it.")
	}
	if *invitePath == "" && !*printInvite {
		fail("one of -invite or -printinvite is required: an invite nobody receives joins nobody.")
	}

	server, err := connect.ParseId(*serverId)
	if err != nil {
		fail("parse server id %q: %v", *serverId, err)
	}

	// Ctrl-C has to reach the closes below, not kill the process on top of an open state store:
	// the store holds an exclusive file lock and a process killed with it open leaves the next run
	// reporting "the directory is held".
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	go func() {
		<-interrupted
		fmt.Printf("\ninterrupted; closing the state store cleanly\n")
		cancel()
	}()

	p := dial(ctx, *jwtPath, *host, server, *dir, *timeout, *reconnect, *route, *endpoint, *pin)
	defer p.close()

	// ── 1. Hello ───────────────────────────────────────────────────────────────────────────
	say("Hello, and what the server advertises")
	// RETRIED ACROSS THE OPERATOR'S RECONNECT WINDOW, and this used to be a single shot that
	// could not survive a restart.
	//
	// A client_id that has just re-dialled is not routed to for about sixty seconds, so the FIRST
	// Hello after any restart is answered by nobody. dial() above builds the device with a
	// urmessage.ConnectPolicy carrying -reconnect as its budget -- but that policy belongs to
	// Device.Connect, which is step 2, and THIS call goes straight at the transport. So the budget
	// was wired, correct, and unreachable: every restart inside the window died here with
	// "no response carrying this request_id arrived before the deadline", one minute after launch,
	// before the retrying code was ever reached. Measured twice in a row while a person waited on
	// the far side of a conversation this process was supposed to be holding up.
	//
	// THE BUDGET IS THE SAME ONE, so -reconnect means one thing across this binary. Zero takes a
	// default comfortably past the window rather than failing on the first attempt, because the
	// window is a property of the operator and not of how this run was invoked.
	helloBudget := *reconnect
	if helloBudget <= 0 {
		helloBudget = 3 * time.Minute
	}
	var reason protocol.Reason
	var hello *protocol.HelloResponse
	helloStarted := time.Now()
	for attempt := 1; ; attempt += 1 {
		hctx, hcancel := context.WithTimeout(ctx, *timeout)
		var err error
		// THE SECOND ARGUMENT IS THE PROTOCOL VERSIONS THIS CLIENT SPEAKS, NOT A REQUEST ID, and
		// it is 1 on EVERY attempt. The first cut of this loop passed the attempt counter, so the
		// retry that finally got through the reconnect window announced "I speak v2" and the
		// server answered REASON_UNSUPPORTED_VERSION -- correctly -- one round trip after the
		// fix had done its job. Hello(ctx, versions ...uint32) is the signature; the request id
		// is the transport's own, assigned per call.
		reason, hello, err = p.transport.Hello(hctx, 1)
		hcancel()
		if err == nil {
			if 1 < attempt {
				fmt.Printf("  answered on attempt %d, after %v\n", attempt, time.Since(helloStarted).Round(time.Second))
			}
			break
		}
		if helloBudget <= time.Since(helloStarted) {
			fail("hello: %v\n"+
				"  Unanswered for %v, which is past -reconnect. A client_id that has just re-dialled\n"+
				"  is not routed to for about a minute; if this run followed another under the same\n"+
				"  account, give it longer or wait before starting it.", err, helloBudget)
		}
		fmt.Printf("  reconnecting: Hello attempt %d was not answered (%v); waiting 5s\n", attempt, err)
		select {
		case <-ctx.Done():
			fail("hello: cancelled while waiting out the reconnect window")
		case <-time.After(5 * time.Second):
		}
	}
	if reason != protocol.Reason_REASON_OK {
		fail("Hello answered %v, want REASON_OK", reason)
	}
	capabilities := hello.GetCapabilities()
	fmt.Printf("  nonce %d octets; max_records_per_fetch=%d max_request_bytes=%d attestation_supported=%v\n",
		len(hello.GetServerNonce()), capabilities.GetMaxRecordsPerFetch(),
		capabilities.GetMaxRequestBytes(), capabilities.GetAttestationSupported())

	// Device.Connect rides the operator's reconnect window: a client_id that has just re-dialled
	// is NOT ROUTED TO for about sixty seconds, and a request sent into that window is LOST rather
	// than queued. Only re-sending gets through, which is what the policy below does.
	connectStarted := time.Now()
	if err := p.device.Connect(ctx); err != nil {
		if errors.Is(err, urmessage.ErrReconnecting) {
			fail("still not routed to after %v of retrying: %v\n"+
				"  THIS IS THE OPERATOR'S RECONNECT WINDOW (~60s) AND NOT A BROKEN CREDENTIAL.\n"+
				"  Raise -reconnect above the window and run again.",
				time.Since(connectStarted).Round(time.Second), err)
		}
		fail("Connect: %v", err)
	}
	fmt.Printf("  routed to after %v\n", time.Since(connectStarted).Round(time.Millisecond))

	// ── 2. restore, or do the handshake ────────────────────────────────────────────────────
	//
	// A SECOND RUN OVER THE SAME -dir MUST NOT FOUND A SECOND GROUP. The app joined once and
	// restores from its own disk on every later launch, so it publishes NO key package after the
	// first time -- a helper that founded again would wait for one that is never coming. Restoring
	// is also what makes this useful while the app is up: it sends into the conversation the app is
	// already showing.
	say("restoring any group this peer already holds")
	restored, err := p.device.Restore(ctx)
	if err != nil {
		fmt.Printf("  restore reported: %v (and answered %d group(s))\n", err, len(restored))
	}
	if 0 < len(restored) {
		group := restored[0]
		fmt.Printf("  restored group %s at epoch %d, open=%v -- SKIPPING the handshake\n",
			hex.EncodeToString(group.Id()), group.Epoch(), group.IsOpen())
		// A RESTORED GROUP WILL NOT SEAL UNTIL Receive HAS RUN ONCE OVER IT: the cursor is not
		// persisted, so this re-reads the history and re-derives the keys its own next send needs.
		if got, err := group.Receive(ctx); err != nil {
			fmt.Printf("  the restored group's first receive reported: %v (%d record(s))\n", err, len(got))
		} else {
			fmt.Printf("  the restored group read %d record(s) back\n", len(got))
		}
		converse(ctx, group, *serve, *poll, *outbox, *quiet)
		report(group)
		return
	}
	fmt.Printf("  nothing to restore; this is a first run and the handshake below is how the app\n" +
		"  gets into a group at all\n")

	var keyPackage []byte
	if *joinCode != "" {
		say("reading the join code")
		text := *joinCode
		if strings.HasPrefix(text, "@") {
			raw, err := os.ReadFile(strings.TrimPrefix(text, "@"))
			if err != nil {
				fail("read join code file: %v", err)
			}
			text = string(raw)
		}
		// WHITESPACE OUT FIRST. A code that has been through a mail client or a chat window comes
		// back wrapped, and refusing it would blame the person for their mail client.
		text = strings.Join(strings.Fields(text), "")
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			fail("that join code is not base64: %v\n"+
				"  Copy the WHOLE code out of the app - it is one long line.", err)
		}
		keyPackage = decoded
		fmt.Printf("  %d characters decoded to %d octets\n", len(text), len(keyPackage))
		fmt.Printf("  A JOIN CODE IS NOT A SECRET: it is a public offer to be added, and whoever\n" +
			"  holds it can only add that device to a group. The INVITE below is the secret.\n")
	} else {
		say("waiting for the app's key package at " + *keyPackagePath)
		fmt.Printf("  THE APP PUBLISHES IT. Launch URmessage.exe with --live; it writes its key package\n" +
			"  here when it holds no group, then polls for the invite this helper writes back.\n")
		raw, err := awaitFile(ctx, *keyPackagePath, *waitFor, *poll)
		if err != nil {
			fail("no key package: %v\n"+
				"  Nothing here mints one: it is the APP's device identity and only the app's device\n"+
				"  holds the private halves a Welcome addressed to it is opened with.", err)
		}
		keyPackage = raw
		fmt.Printf("  read %d octets\n", len(keyPackage))
		// CONSUMED AND DELETED. A key package is single use; leaving the file behind invites a
		// second run to build an invite the app cannot open.
		if err := os.Remove(*keyPackagePath); err != nil {
			fmt.Printf("  WARNING: the key package file could not be removed (%v). Remove it by hand:\n"+
				"  a stale one read by a later run builds an invite that will not join.\n", err)
		}
	}

	// ── 3. found, add, open ────────────────────────────────────────────────────────────────
	say("founding the group, adding the app, and publishing it on the server")
	groupId := make([]byte, urmessage.GroupIdBytes)
	if _, err := rand.Read(groupId); err != nil {
		fail("group id: %v", err)
	}
	group, err := p.device.CreateGroup(ctx, groupId)
	if err != nil {
		fail("CreateGroup: %v", err)
	}
	if group.Epoch() != 0 {
		fail("a freshly founded group is at epoch %d, want 0", group.Epoch())
	}
	invite, err := group.AddMember(keyPackage)
	if err != nil {
		fail("AddMember: %v\n"+
			"  If this names a stale or already-used key package, the app has already joined off\n"+
			"  this one. Delete the app's live state directory, relaunch it, and run this again.", err)
	}
	if group.Epoch() != 1 {
		fail("the commit that added the app left the group at epoch %d, want 1", group.Epoch())
	}
	encoded, err := invite.Encode()
	if err != nil {
		fail("encode invite: %v", err)
	}
	if err := group.Open(ctx); err != nil {
		fail("Open: %v", err)
	}
	if !group.IsOpen() {
		fail("Open returned no error and the group does not say it is open")
	}
	fmt.Printf("  group %s at epoch %d, open on the server\n", hex.EncodeToString(group.Id()), group.Epoch())

	// ── 4. hand the invite over ────────────────────────────────────────────────────────────
	if *invitePath != "" {
		say("writing the invite to " + *invitePath)
		fmt.Printf("  THIS FILE IS KEY MATERIAL IN FULL: whoever reads it is in this group. It is written\n" +
			"  0600 and the app deletes it the moment it has joined.\n")
		if err := writeSecret(*invitePath, encoded); err != nil {
			fail("write invite: %v", err)
		}
		fmt.Printf("  %d octets written\n", len(encoded))
	}
	if *printInvite {
		say("the INVITE CODE - paste this back into the app that sent the join code")
		fmt.Printf("  WHOEVER READS THE NEXT LINE IS IN THIS GROUP. Send it the way you would send a\n" +
			"  password, give it to ONE person, and do not post it where a copy is kept. It can\n" +
			"  be used once.\n\n")
		code := base64.StdEncoding.EncodeToString(encoded)
		fmt.Println(code)
		fmt.Printf("\n  %d octets as %d characters\n", len(encoded), len(code))
	}

	// ── 5. wait for the app to actually be in the group ────────────────────────────────────
	// THE APP DELETING THE INVITE IS THE ONLY SIGNAL THERE IS. A member joining is invisible from
	// here: MLS's Welcome is opened by the joiner and the server is told nothing. So this waits on
	// the file going away rather than pretending to observe a membership.
	// AND IT IS ONLY A SIGNAL WHEN THERE IS A FILE. With -printinvite and no -invite there is
	// nothing on disk to go away, and awaitGone answers true immediately for an empty path - so
	// the old line printed "the app has read it" about a file that was never written. That is a
	// pass with no subject: the strongest thing it could have meant is still unobserved, and
	// saying it anyway is worse than saying nothing. The code road has no confirmation here at
	// all, and the honest report is to say which road this run took and what it did not see.
	if *invitePath != "" {
		say("waiting for the app to consume the invite")
		joined := awaitGone(ctx, *invitePath, *waitFor, *poll)
		if joined {
			fmt.Printf("  the invite file is gone, so the app has read it\n")
		} else {
			fmt.Printf("  WARNING: the invite is still on disk. The sends below still happen and are\n" +
				"  still readable whenever the app does join - a record sealed at epoch 1 opens for\n" +
				"  every member of epoch 1, whenever they arrive. Delete it by hand when done.\n")
		}
	} else {
		say("the invite was printed, not written, so nothing here can see it being used")
		fmt.Printf("  A MEMBER JOINING IS INVISIBLE FROM THIS SIDE: MLS's Welcome is opened by the\n" +
			"  joiner and the server is told nothing. With a file there is at least a deletion to\n" +
			"  watch; with a pasted code there is not. What WILL show that they joined is a line\n" +
			"  from them arriving in the fetch below, and nothing before that.\n")
	}

	// ── 6 and 7: the half a restored run shares with this one ──────────────────────────────
	converse(ctx, group, *serve, *poll, *outbox, *quiet)
	report(group)
}

// converse sends the real messages and then fetches in a loop.
//
// IT IS A FUNCTION BECAUSE A RESTORED RUN NEEDS EXACTLY THIS AND NONE OF THE HANDSHAKE. The
// handshake happens ONCE in the life of a pairing; everything interesting happens here, on every
// run.
func converse(ctx context.Context, group *urmessage.Group, serve time.Duration, poll time.Duration, outbox string, quiet bool) {
	// EVERY RUN'S FIRST LINE IS DISTINCT. A second run that re-sent the same three strings would be
	// indistinguishable, on the far side, from the first run's still being there -- so the clock is
	// in the text and a new line is visibly new.
	// THE SCRIPT IS SKIPPABLE. Its three lines, the reply and the two reactions exist to prove the
	// kinds cross. A run whose job is to TALK does not want them: the person on the far side would
	// have to scroll past a demo to find the conversation.
	if !quiet {
		stamp := time.Now().Format("15:04:05")
		say("sending real messages: texts, a reply, and a reaction")
		lines := []string{
			"Hello from the second device (" + stamp + ") — this line was sealed on another machine.",
			"This is the URmessage alpha over the real mesh: two accounts, two clients, one group.",
			"Nothing on this screen was written by the app you are looking at.",
		}
		var anchorId []byte
		for at, line := range lines {
			sent, err := group.Send(ctx, line)
			if err != nil {
				fail("Send line %d: %v", at+1, err)
			}
			fmt.Printf("  sent record %d message %s: %q\n", sent.RecordId, short(sent.MessageId), line)
			if at == 0 {
				anchorId = append([]byte(nil), sent.MessageId...)
			}
		}

		// A REPLY carries its parent's NAME and never its text: a receiver renders it by looking the
		// parent up. Pointing it at this helper's own first line means the app has both halves and can
		// be seen to have resolved the reference rather than to have carried it.
		const replyText = "…and this one is a reply to the first line, so the app has a parent to resolve."
		replySent, err := group.SendReply(ctx, anchorId, replyText)
		if err != nil {
			fail("SendReply: %v", err)
		}
		fmt.Printf("  sent a REPLY, record %d, naming %s\n", replySent.RecordId, short(anchorId))

		// A REACTION adds no line of its own: it changes a line that is already there. What comes back
		// is the reaction's own record id, not a row to render.
		for _, emoji := range []string{"👍", "🎉"} {
			reacted, err := group.React(ctx, anchorId, emoji)
			if err != nil {
				fail("React %q: %v", emoji, err)
			}
			fmt.Printf("  sent a REACTION %s, record %d\n", emoji, reacted.RecordId)
		}

		// serve: fetch in a loop so the app's own lines are seen here too
	}

	say(fmt.Sprintf("fetching every %v for %v, printing what arrives", poll, serve))
	fmt.Printf("  THERE IS NO PUSH. This transport is a poll and so is the app's; a line the app\n" +
		"  sends becomes visible here on the next fetch and not before.\n")
	// THE ROSTER, AS THIS PEER READS IT, at the start and again after every commit it ingests:
	// a role change the app makes -- promoting this peer, say -- lands here as a policy commit
	// on some later fetch, and the row that changes is the proof that the far side's view moved.
	printRoster(group, "at the start of serving")
	ingested := group.Stats().Ingested
	deadline := time.Now().Add(serve)
	seen := map[string]bool{}
	// How far into the outbox file this run has already sent. Zero at start, so a file left over
	// from an earlier run IS re-sent -- which is why the caller gives each run a fresh one.
	var outboxAt int64
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			fmt.Printf("  cancelled\n")
			return
		case <-time.After(poll):
		}
		// THE OUTBOX, read before the fetch so a line written while this slept goes out on this
		// turn rather than the next. BY OFFSET, not by diffing the whole file: the file only ever
		// grows, and re-reading it whole would re-send everything after any edit.
		if outbox != "" {
			if raw, err := os.ReadFile(outbox); err == nil && int64(len(raw)) > outboxAt {
				fresh := string(raw[outboxAt:])
				outboxAt = int64(len(raw))
				for _, line := range strings.Split(fresh, "\n") {
					line = strings.TrimRight(line, "\r")
					if strings.TrimSpace(line) == "" {
						continue
					}
					sendStarted := time.Now()
					sent, err := group.Send(ctx, line)
					submitMs := time.Since(sendStarted).Milliseconds()
					if err != nil {
						// NOT fail(): a refused send must not end a conversation. Say so and keep serving.
						fmt.Printf("  -> REFUSED: %v\n", err)
						continue
					}
					// THE SUBMIT IS A ROUND TRIP TO THE SERVER AND IS TIMED SEPARATELY from when the far
					// side SEES it. Those are two different numbers and conflating them is how "the app
					// feels slow" becomes unanswerable: a 90ms submit followed by a 3s poll is a fast
					// protocol with a coarse clock, not a slow network.
					fmt.Printf("  [%s] -> sent record %d in %dms: %q\n",
						time.Now().Format("15:04:05.000"), sent.RecordId, submitMs, line)
				}
			}
		}
		got, err := group.Receive(ctx)
		if err != nil {
			// A NON-EMPTY RESULT AND AN ERROR CAN BOTH COME BACK, so the messages are printed
			// before the reason rather than instead of it.
			fmt.Printf("  receive reported: %v (and still answered %d record(s))\n", err, len(got))
		}
		if now := group.Stats().Ingested; now != ingested {
			printRoster(group, fmt.Sprintf("after ingesting commit %d", now))
			ingested = now
		}
		for _, one := range got {
			key := hex.EncodeToString(one.MessageId)
			if seen[key] {
				continue
			}
			seen[key] = true
			fmt.Printf("  [%s] <- record %d from %s kind=%s deleted=%v reply_to=%s: %q\n",
				time.Now().Format("15:04:05.000"),
				one.RecordId, short(one.SenderHandle), one.Kind, one.Deleted,
				short(one.ReplyToId), clip(one.Text))
		}
		// Reactions land on a message that is ALREADY in the log, so they are never in what
		// Receive answers. They are read off the log.
		for _, one := range group.Messages() {
			if len(one.Reactions) == 0 {
				continue
			}
			key := "reactions:" + hex.EncodeToString(one.MessageId) + ":" + reactionKey(one)
			if seen[key] {
				continue
			}
			seen[key] = true
			emojis := []string{}
			for _, reaction := range one.Reactions {
				emojis = append(emojis, fmt.Sprintf("%s(mine=%v)", reaction.Emoji, reaction.Mine))
			}
			fmt.Printf("  <- REACTIONS now standing on %s: %s\n",
				short(one.MessageId), strings.Join(emojis, " "))
		}
	}
}

// ── the party ────────────────────────────────────────────────────────────────────────────────

// dial is liveprobe's dial with one party instead of three. It calls sdk.NewMessageClient, which is
// the ONE construction of a platform-attached client in this workspace — the same one the C ABI the
// Windows app links calls — so a run of this is evidence about the code the app links rather than
// about a copy of it.
func dial(ctx context.Context, jwtPath string, host string, server connect.Id, root string,
	timeout time.Duration, reconnect time.Duration, route string, endpoint string, pin string) *party {
	raw, err := os.ReadFile(jwtPath)
	if err != nil {
		fail("read jwt: %v", err)
	}
	// THE CREDENTIAL IS A SECRET AND THIS IS THE ONLY PLACE IT IS HELD. It goes to
	// NewMessageClient and nowhere else; it is never printed, and the only thing said about it is
	// its length.
	byJwt := strings.TrimSpace(string(raw))
	fmt.Printf("credential read from %s (%d bytes; its contents are never printed)\n", jwtPath, len(byJwt))

	client, closer, described := routeClient(ctx, route, byJwt, host, endpoint, pin, "urmessage-livepeer")
	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client: client, Server: server, ProtocolVersion: 1, Timeout: timeout,
	})
	if err != nil {
		fail("transport: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "stream"), 0o700); err != nil {
		fail("stream dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		fail("state dir: %v", err)
	}
	streamStore, err := sdk.OpenStreamStore(filepath.Join(root, "stream"))
	if err != nil {
		fail("OpenStreamStore: %v", err)
	}
	stateStore, err := urmessage.OpenDurableStateStore(filepath.Join(root, "state"))
	if err != nil {
		fail("OpenDurableStateStore: %v -- if this says the directory is held, another run of this helper is still alive", err)
	}
	device, err := urmessage.NewDevice(urmessage.DeviceConfig{
		Transport:  transport,
		Reserver:   sdk.NewStreamIndexReserver(streamStore),
		StateStore: stateStore,
		Connect: urmessage.ConnectPolicy{Budget: reconnect, OnAttempt: func(attempt urmessage.ConnectAttempt) {
			fmt.Printf("  reconnecting: Hello attempt %d was not answered after %v; waiting %v (%v)\n",
				attempt.Attempt, attempt.Elapsed.Round(time.Millisecond),
				attempt.Backoff.Round(time.Millisecond), attempt.Err)
		}},
	})
	if err != nil {
		fail("NewDevice: %v", err)
	}
	fmt.Printf("%s, durable state in %s\n", described, root)
	return &party{client: closer, transport: transport, streamStore: streamStore,
		stateStore: stateStore, device: device}
}

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

// ── the file seam ────────────────────────────────────────────────────────────────────────────

// awaitFile polls for a file and answers its contents. It requires the file to be NON-EMPTY and to
// have stopped growing between two polls, because the writer on the other side is a different
// process and a partially written file read whole is a checksum failure at the parse rather than
// here, where it can be waited out.
func awaitFile(ctx context.Context, path string, within time.Duration, poll time.Duration) ([]byte, error) {
	deadline := time.Now().Add(within)
	var previous []byte
	announced := false
	for {
		raw, err := os.ReadFile(path)
		if err == nil && 0 < len(raw) {
			if previous != nil && bytes.Equal(previous, raw) {
				return raw, nil
			}
			previous = raw
		}
		if !announced {
			fmt.Printf("  polling every %v, up to %v\n", poll, within)
			announced = true
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%s did not appear within %v", path, within)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// awaitGone polls for a file to be removed. False means it timed out, which is a report and not a
// failure: the sends after it are readable by a member whenever that member arrives.
func awaitGone(ctx context.Context, path string, within time.Duration, poll time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(poll):
		}
	}
}

// writeSecret writes 0600, via a temp file and a rename, so a reader polling the path never sees a
// half-written invite. The temp file is created in the destination's own directory: a rename across
// volumes is not atomic and on Windows is not a rename at all.
func writeSecret(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp := path + ".partial"
	if err := os.WriteFile(temp, content, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}

// ── the harness ──────────────────────────────────────────────────────────────────────────────

func say(s string) { fmt.Printf("\n=== %s ===\n", s) }

// printRoster is this peer's Members() -- every leaf, its role, its identity's first octets and
// whether it is this device -- with the epoch it was read at, and this peer's own role beside it.
func printRoster(group *urmessage.Group, when string) {
	members, err := group.Members()
	if err != nil {
		fmt.Printf("  <- ROSTER %s could not be read: %v\n", when, err)
		return
	}
	myRole, _ := group.MyRole()
	rows := []string{}
	for _, member := range members {
		mine := ""
		if member.Mine {
			mine = " (this peer)"
		}
		rows = append(rows, fmt.Sprintf("leaf %d %s %s%s", member.LeafIndex, member.Role, short(member.IdentityPub), mine))
	}
	fmt.Printf("  <- ROSTER %s, epoch %d, my role %s: %s\n", when, group.Epoch(), myRole, strings.Join(rows, "; "))
}

func short(id []byte) string {
	if len(id) == 0 {
		return "-"
	}
	if 8 < len(id) {
		return hex.EncodeToString(id[:8])
	}
	return hex.EncodeToString(id)
}

func clip(text string) string {
	if 100 < len(text) {
		return text[:100] + fmt.Sprintf("…(%d octets)", len(text))
	}
	return text
}

func reactionKey(one *urmessage.Message) string {
	parts := []string{}
	for _, reaction := range one.Reactions {
		parts = append(parts, reaction.Emoji)
	}
	return strings.Join(parts, ",")
}

func report(group *urmessage.Group) {
	stats := group.Stats()
	fmt.Printf("\n=== the counters ===\n")
	fmt.Printf("  fetched=%d opened=%d ceremony=%d own=%d otherClasses=%d FAILED=%d submitted=%d rebound=%d pages=%d unattested=%d\n",
		stats.Fetched, stats.Opened, stats.SkippedCeremony, stats.SkippedOwn,
		stats.SkippedClass, stats.FailedOpen, stats.Submitted, stats.Rebound, stats.Pages, stats.Unattested)
	fmt.Printf("  the group's whole log holds %d message(s)\n", len(group.Messages()))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\nFAIL: "+format+"\n", args...)
	os.Exit(1)
}
