# liveprobe

Three real URnetwork accounts, three real platform connections, one **deployed** message server —
and the whole of what the alpha can do, scenario by scenario, including the membership change
that makes a group chat a group chat.

Every other transport test in this workspace runs two `connect.Client`s over in-process
`connect.Route` channels in one binary — including `cp3b`, which is where the key schedule is
actually proven. This one crosses an operator's mesh to a server it did not start, so it is the
only thing here that can find a defect that lives in the gap between them. It found the first
one: a message server that enables no provide mode accepts no peer's frames, logs nothing, and
answers `ready` the whole time.

It needs credentials and a server, so it is a command rather than a test: `go test ./...` on a
developer's machine must not require an operator account.

    liveprobe \
      -a  <file holding party A's by_client_jwt> \
      -b  <file holding party B's by_client_jwt> \
      -c  <file holding party C's by_client_jwt> \
      -server <the message server's client_id> \
      -host beta-test.net \
      -dir /var/lib/urmessage/probe-2026-09-29

Each `by_client_jwt` is a `network_client` credential minted by `POST /network/auth-client`
against that operator, per spec B §9.1. **They are secrets**: pass paths, never values, and keep
the files at mode 600.

**Which account plays which role**, because after step 10 they are not interchangeable:

| flag | party | what it is for |
|---|---|---|
| `-a` | **A** | founds the group; the OWNER until step 10's transfer; step 11 demotes it to MEMBER, has it refused a removal, takes it **offline across the removal** and brings it back to converge. |
| `-b` | **B** | the second member; step 7 **kills and restarts** it; step 10's transfer makes it the OWNER; step 11 has it **commit the removal**. |
| `-c` | **C** | the third member step 5 adds; step 10 demotes it to OBSERVER; step 11 makes it a MEMBER again, gives it a **second device leaf**, and **removes it** — so it is the party that must answer the removed sentinel and still answer it after a restart. |

`-dir` is where each party's **durable** state lives, and it now matters twice: step 7 kills a
client and starts it again over the same directory, and step 11 does it to two more. **Use a FRESH,
EMPTY directory for every run** — a second run over one `-dir` fails at step 7 with "B was in one
group and N came back from the disk", because `Restore` brings back every group the directory
holds. Use a path that survives the run, and note that each party holds a single-writer exclusion on
its own subdirectory — if a second probe is refused with "the state directory is held", a previous
run is still alive.

**A directory written before the X-Wing wrap seed existed is refused BY NAME at dial time**, before
any step spends a round trip. Such a directory holds a three-part identity record and is answered
with a nil error and an *empty* seed (urmessage's `DurableStateStore.GetDeviceIdentity` says why it
is not refused there), so the device publishes a leaf whose private half nothing can reconstruct and
can follow no epoch anybody else opens. The deployed alpha's own device directories are three-part
ones — ledger item 257, whose **ruling 53 is to re-found that deployment**. This matters most to
step 11: a device that derives no epoch at all would *satisfy* "the removed member cannot derive the
epoch its own removal opened", so that clause would pass vacuously on the one party it is about.
Hence the named refusal rather than a late, generic "a wrap did not open".

## The eleven steps, and what each one is for

1. **Hello on both parties.** A 32-octet `server_nonce`, and the capabilities the server
   advertises are PRINTED — `max_records_per_fetch`, `max_request_bytes`,
   `attestation_supported`. The fetch-page step needs the first of those to be set sensibly, and
   the third is the one that is false on the deployed server today.
2. **A founds a group, adds B, opens it.** Epoch 0 → the commit → §6.1's founding commit, the wrap
   set, the marker.
3. **B joins and reads the string A typed**, and answers it. This is what CP3b proved in one
   process; here it crosses the mesh.
4. **`-lines` messages in order** (default 600), which is what crosses a fetch page. §4.3.4
   truncates a page by `limit` **or** by `max_response_bytes` and calls both NORMAL; `Receive`
   must page until the server says `complete`. The step prints the page count, and **warns loudly
   if one page carried everything** — in which case `-lines` is below this server's
   `max_records_per_fetch` and the truncation path was not exercised.
5. **A third member is added to a group that has been chatting — a second epoch, live.** Everything
   above ran at epoch 1. A calls `AddMemberAndPublish` on the OPEN group with C's key package:
   a commit sealed at epoch 1 announcing epoch 2, the wrap fan-out for the new epoch and the marker,
   all submitted to the deployed server. C joins from the Welcome at epoch 2. B, who authored
   nothing, INGESTS the commit on its next `Receive` and follows into epoch 2 — asserted on B's own
   counters (`Stats.Ingested == 1`, `Epoch() == 2`, and zero `out_of_window` gaps, because a member
   that was current loses nothing). C drains the pre-join history: every epoch-1 line comes back as
   an `out_of_window` GAP, counted exactly against the lines exchanged at epoch 1 (`2 + -lines`),
   not tolerated. Then one line from each of A, B and C opens on both others — six directions,
   each asserted on the FAR side against the exact text and against being a line rather than a gap.
   `-c` is required; the founding-time `AddMember` still refuses a second add by name and this is
   the other door. (In-process, this is `cp3b`'s
   `TestThreeDevicesConvergeToEpochTwoAndExchangeMessagesEveryDirection`.)
6. **A `-big` octet message** (default 40000), which crosses §4.6's 2048-octet cut as roughly
   twenty frames and is reassembled on the far side. No live test had ever fragmented. The text is
   compared octet for octet and the step prints the offset of the first difference if there is one.
7. **B is killed mid-conversation and started again over the same directory.** This is S2-14. The
   device, both durable stores, the transport and the connect client are all dropped and a new
   everything is opened; the only thing that crosses is the disk. It holds that B comes back into
   the same group at the same epoch under the same leaf, opens a record A sealed **before** the
   restart, **reads back the two lines B ITSELF sent before the kill**, and seals one A opens
   after it. That middle clause is new and it is the one that used to be missing: a restored
   group's log starts empty and `Receive` skipped every record whose `sender_handle` was its own,
   so a user who closed the app and reopened it got the other side's half of the conversation and
   none of their own — with a nil error. A probe that checks only the far side's half cannot see
   that, and this one could not.

   **And the pre-change history is counted, not hidden.** The restarted B re-walks its whole
   history at epoch 2 with a single-epoch session, so every epoch-1 line — A's and B's own alike —
   comes back as an `out_of_window` gap. The step asserts that count EXACTLY against the lines the
   group exchanged at epoch 1, that exactly one of them is B's own (step 3's answer), that
   `Stats.GapOutOfWindow` agrees with the walk, and that no gap of any other reason appeared; then
   it prints the number. That number is what item 241's history-across-a-membership-change will
   one day carry instead.

   **This step lands in the operator's reconnect window every time, and that is expected.**
   Measured on the deployed server: a `client_id` that has just re-dialled **is not routed to for
   about sixty seconds** — the connection attaches, the Hello goes out and nothing comes back.
   Step 7 re-dials B under the same `client_id`, so its `Connect` meets that window on every run.
   `Device.Connect` now **retries with backoff across it** and prints `reconnecting: Hello attempt
   N ...` for each unanswered try, so the step takes up to a minute and says why rather than
   failing. `-reconnect <duration>` raises the budget above urmessage's 90s default. If it ends in
   `ErrReconnecting` the window outlasted the budget — **that is the operator finding (item 5 of
   `msgrepo/docs/reports/2026-09-15-operator-and-connect-findings.md`), not a restore failure**,
   and the step says so by name. None of this removes the sixty seconds; the user waits them.
8. **Two senders at once.** Both parties send 20 lines concurrently. Each line is distinct, so a
   lost one and a duplicated one are both visible in the counts.
9. **The content envelope over the mesh: a reply, two reactions, one taken back, and a delete.**
   Every kind is asserted on the FAR side -- the reply names the anchor's `message_id`, the
   reactions land on the anchor rather than as lines, the un-reaction leaves exactly one
   standing, B is refused a tombstone for A's line on the send side, and A's own tombstone marks
   B's copy without removing it.
10. **Roles: a promotion, a member's refused add, a transfer of ownership and a demotion.** MASTER
    §11's role model (ledger item 242) over the deployed server with three real devices. A, the
    founder and OWNER, promotes B to ADMIN; B and C ingest the policy commit and every party's
    `Members()` reads A owner / B admin / C member. C, a MEMBER, calls `AddMemberAndPublish`
    with a fresh stranger's key package and is **refused on the send side**
    (`ErrCommitUnauthorized` wrapping `ErrCommitAddByNonAdmin`), `Stats.CommitRefusedOwn` moves
    by one, and **no party's epoch moves** -- A and B fetch nothing. Then A transfers ownership to
    B: every roster reads B owner / A admin (ruling 4) / C member, and `MyRole()` agrees on both.
    Then B, the new owner, demotes C to OBSERVER and A -- now an admin -- follows a commit it did
    not make. A roles table is printed per party at every stage, off each party's OWN roster with
    its epoch, and any disagreement between the three is a `FAIL` line naming the party, the
    identity and both roles. (In-process, this is `cp3b`'s `TestRolesConvergeAcrossThreeDevices`.)
11. **A REMOVAL on the real mesh: the OWNER takes a member's two devices out in one commit, a
    survivor that was offline across it converges, and the removed device is told so and stays
    told.** This is the removal track (ledger items 257–259), and none of it had ever crossed an
    operator's mesh: the derivation and the refusals are held in `urmessage`, the submit and the
    convergence in `cp3b`'s
    `TestOneCallRemovesEveryLeafOfOneIdentityAndTheRemovedMemberCannotFollow` and
    `TestARemovedDeviceIsToldSoByNameOnEveryWalkAndStillIsAfterARestart`. Twelve stages, and each
    of the six properties has a way to fail that one process cannot show:

    - **Stage 0 — the dial-time seed check's own control, both ways.** A device whose seed has been
      erased answers `ErrNoDeviceWrapKey`; a live party answers a *different* error to the same
      empty ciphertext. Without both arms the refusal described above is either dead or fires for
      everything.
    - **Stages 1–3 — the cast.** B, the owner, makes C a MEMBER again (so the removal is of a
      member, and C's own `Send` in stage 7 is refused for the *removal* and not for being an
      observer's); **C adds its OWN second device leaf** and commits that add itself (MASTER §11's
      self-service rule, and R6a requires an Add claiming an identity already in the group to be
      committed BY that identity); B demotes A to MEMBER. Every party's roster then reads **four
      rows and three identities**, with C's identity at **two leaves** and each survivor at one —
      the control the whole step rests on, since an identity with one leaf cannot tell a
      per-identity removal from a per-leaf one.
    - **Stage 4 — the send-side refusals, while the victim is still a member.** A, a MEMBER, is
      refused `RemoveMember(C)` with `ErrCommitUnauthorized` wrapping R2's
      `ErrCommitRemoveByNonAdmin` and `Stats.CommitRefusedOwn` moves by one. Then the three
      by-name doors, and **ruling 55's order is asserted rather than relied on**: a MEMBER asking
      to remove the OWNER is answered `ErrRemoveOwner` — "nobody removes the owner, transfer
      first" — and **not** R2, because the subject-level doors come before the predicate. Removing
      its own identity is `ErrRemoveSelf`, an identity with no leaf is `ErrNoSuchMember`, and
      neither is counted as a role refusal. Then **nothing reached the wire**, at every party: no
      entry, no epoch move, no ingest, and no *receiving*-side refusal either, since one would be
      evidence that a commit the send side should have refused reached the server.
    - **Stage 5 — a survivor goes OFFLINE, across the removal.** Everything A holds is dropped and
      its directory is left alone. This is the case every real group chat meets on its first day
      and no in-process test can: the member that was not there.
    - **Stage 6 — the removal.** One call, by identity. B's epoch moves by one, **both** of C's
      leaves are gone, each survivor still holds exactly one, and the roster's roles say the policy
      entry went with the leaves (a commit that left C named would be an R0c phantom every honest
      receiver refuses).
    - **Stages 7 and 9 — the removed device's own client, and item 246's ceiling.** Four walks and
      a `Send`, each answering `urmessage.ErrRemovedFromGroup` **wrapping mls's own sentinel** and
      **none of the eight other states** ruling 52 says it must be distinguishable from — the halt,
      the dark group, an abandoned record, a transport refusal, a group that has not reconciled.
      `Stats.FailedOpen` does not move and nothing is abandoned, because a removal is the one
      record a device cannot open and must not retry. It still reads its own pre-removal
      transcript. Then B seals three lines at the epoch the removal opened and the removed
      device's **fetched-record delta per walk does not change** — which is F0's ceiling measured
      on the server's own pages, and is stronger than "it cannot open them". Its control is in
      stage 11.
    - **Stage 10 — the restart.** The removed device is killed and reopened over the same
      directory, and `Removal()` is read **before its first Receive**: the cursor is not persisted,
      so a state read there came off the disk and nowhere else. Without ruling 52's persist such a
      device comes back reading as caught up and silent.
    - **Stage 11 — the offline survivor returns.** It **drains rather than walking once, and the
      unit is the round trip.** Under item 246's ceiling a reader is served only rows at or below
      the `read_epoch` inside its own request, and the page it gets back is *complete* — the ceiling
      is a filter and not a truncation — so **one `Receive` crosses one epoch and stops**, and a
      party that must cross the epoch the removal opened *and* read what was sealed above it needs
      more than one. Measured against the real `msgrepo` server, in this stage's shape, rather than
      argued: one `Receive` took a survivor one epoch behind *across* the epoch with **0 entries and
      none of the three lines above it**, and the drain settled in **three** round trips — one that
      crosses, one that reads above, one that answers nothing at an epoch it did not move. The
      mutant that keeps the loop honest was driven too: `len(got) == 0` without the epoch clause
      settles on that very first round. `msgrepo` holds the same rule end to end in
      `TestAMemberSeveralEpochsBehindWalksForwardOneEpochPerRoundTrip`, and `cp3b`'s
      `rolesReceiveAll` drains for the same reason and says so. **Exactly one call site in this
      probe drains**, because every other entry assertion is an exact count on a group that is
      already at the head. The **first** round trip is where it must reach the new epoch, having
      ingested **exactly one** commit across the whole drain — the only one above the epoch its disk
      restored, and holding that over a drain is the stronger reading, since a round trip that
      re-applied anything shows up there first — and opened at least one device wrap, because the
      epoch a removal opens is reached only by opening the fan-out wrap addressed to this leaf. Zero
      malformed gaps — the removed member's records sit *below* the removing commit and are its
      whole half of the conversation — and zero out-of-window gaps, per item 241. It **opens every
      one of the three lines B sealed above the ceiling**, which is stage 9's control the other way
      round: without it, a ceiling and a server that served those rows to nobody are the same
      measurement, and the per-round lines the drain prints are what says which of the two a red
      there names — a reader that reached the new epoch and then answered nothing on a further round
      trip is the omission, a reader that never reached it is a convergence failure. Its roster is
      then compared **row by row** against the remover's, and the two exchange a line at the new
      epoch, which one shared `storage_root` is the only way to do.
    - **Stage 12 — the per-party line**, printed the way the counters step prints: epoch, roster
      rows, own role, the removal state in words, and `fetched opened ingested refusedOwn refused
      wraps pastEpoch FAILED gaps`.

    **What this step does NOT do, and why.** The victim's second device is a **leaf**, not a fourth
    `urmessage.Device`: a Device's credential identity *is* its signature key and it mints one
    identity per state store, so that package has no door onto a second leaf of an existing
    identity. What carries one is a seam-level key package whose credential names C's identity and
    whose signer is its own — the same construction as `cp3b`'s `world.seamMemberClaiming`. That
    leaf never connects and never fetches; a fourth *reading* party would need a fourth credential
    and this deployment has three accounts. For the same reason there is **no online non-committer
    survivor**: with three accounts and one victim there are exactly two survivors, and one of them
    is the committer, so the ingest-while-online arm of a removal stays where `cp3b` holds it with
    three survivors in one process.

Then the counters, per party: `fetched opened ceremony own otherClasses FAILED submitted rebound
pages unattested`. **`FAILED` must be 0.**

And last, **this run reads back its own output**. Every octet the probe writes to stdout or stderr is
kept, and the final step asserts that the three-octet prefix every JWT begins with occurs in it
**zero** times, with two controls in the same block: a fabricated JWT-shaped value that the same
scanner must find exactly once, and the transcript's own octet count, so a zero is not a property of
a scanner that matches nothing or of a log that was never written. The three things this binary is
given are bearer credentials for real network clients, its output gets redirected into files and
pasted into tickets, and no reading of the source can promise that no formatted error carried one —
the errors come from four packages `main.go` does not own. The needle itself is deliberately never
printed, so that a success line does not put a hit in the very log an operator greps.

**The same scanner runs inside the failure path, and that is not a nicety.** Every failure here ends
in `os.Exit`, so a scan that lived only in the final step would only ever run over runs in which
nothing failed — while **the print most likely to carry a credential is the FAIL line itself**, for
exactly the reason above. So one scanner has two callers: the final step, which asserts zero, and
`fail`, which scans *after* printing (the transcript tees `errOut`, so by then the FAIL text is
already octets it can see) and, on a non-zero count, prints a loud **treat the credentials as
disclosed and mint replacements** sentence beside the failure. Measured on the built binary with both
arms: `-server not-a-valid-id` fails and prints nothing extra; `-server` given a *fabricated*
JWT-shaped value fails and prints the disclosure, naming the count and never the prefix.

**Stated limit:** it covers every print this file makes and not a library writing to the process's
stdout by its own hand; catching those needs the file descriptor replaced by a pipe, which costs a
drained goroutine and loses whatever is in flight when the probe exits on a failure.

The run therefore prints **13 steps**: the eleven scenario steps, the counters, and the read-back.

## What it does NOT assert

- **That the plaintext is absent from the server's `message_record` rows.** It is (measured on the
  first deployment: 6 rows, 0 matches), but this probe holds no database credential and should
  not. Read it out of the server's database by hand.
- **That the server returned everything it has — in full.** §4.3.4's fetch attestation is the
  Ed25519 signature that would say so, and the deployed server holds no fleet key and signs nothing
  (`msgrepo/api/fetch.go:112`). The client checks the two halves that need no key — a server that
  *advertises* attestation and sends none is refused, and an attestation that describes a
  different page is refused — and **counts** every page whose signature it could not verify in
  `unattested`.

  **The gross case is now caught, and it was free.** §4.3.4's `high_water_record_id` is the
  server's own statement of the highest record it holds for this group, it arrives on every page,
  and it needs no key. A page the server calls `complete` that names a high water above everything
  it handed over is `ErrFetchOmitted`, and this probe fails on it by name. This used to say "a
  server that silently omits records is still undetectable", flat, and that was too strong.

  **What is still undetectable, and is genuinely S2-27's:** a server omitting records from the
  *middle* of a page, and a server that lies about its own high water. Both need the signature
  over the record-id vector and nothing else will do.

  **The false positive, and it is not live yet.** §7.2's retention sweep would prune records out
  from under a high water that is `next_record_id - 1` and never comes down, so a group whose
  oldest records had expired would produce exactly this signal with no dishonesty anywhere. That
  sweep is **not built**: over the server repo, `grep -rn "DELETE FROM" --include=*.go
  --include=*.sql .` answers two lines, both `DELETE FROM migration_audit` in a startup test, and
  nothing deletes a `message_record` row. The `prune_after` column and the sweep's worklist index
  exist and the sweep does not. So today a red run here is an omission; against a server new
  enough to sweep, check its retention settings first.

- **That two copies of a `-dir` are safe.** They are not. A COPIED app-data directory -- a backup
  restored onto a second machine, a `cp -r` of the folder -- is two devices at one leaf with one
  stream counter, and two records under one `(epoch, sender_handle, stream_index)` are one record
  key and one nonce. (This is *not* the same as running the probe twice over one `-dir` in
  sequence: a second run founds a fresh group id, so the old group's indices still match its own
  reserver. What a second run over one `-dir` actually hits is step 7's `B was in one group and N
  came back from the disk`, because `Restore` brings back every group the directory holds. Use a
  fresh `-dir` per run.) A restored group refuses to seal until it has
  compared its stream position against the server, and a group that finds an index its own
  reserver never allocated refuses to seal at all (`ErrIdentityInUse`). That catches every copy
  that is *behind* the original, before it seals. It does not catch two copies that are exactly
  level and both send before either fetches — that is **S2-28**, and closing it needs a new leaf
  for the copy, which is an MLS Update commit a restored group cannot make.

## Building it

There is no committed binary and there should not be: this probe's audience is an operator on a
deployed Linux VPS, and the `windows/amd64` executable that used to be tracked here was 35 MB of
git weight that served nobody, had to be rebuilt anyway, and was the single artefact in this tree
most likely to be picked up and run by mistake. It also went stale the moment `main.go` changed.

```sh
cd sdk/liveprobe
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o liveprobe .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o liveprobe-arm64 .
```

`CGO_ENABLED=0` is what makes the result a static binary an operator can copy onto a host that has
no toolchain on it.
