// Package urmessage is the send/receive seam: the join between connect/messagegroup's MLS, this
// module's durable stream store and §10.1 transport, and a real message server.
//
// It is the alpha and it is deliberately small. A group exists, a second device joins it by
// Welcome, a line of text is sealed as a DURABLE record and submitted, and the other device
// fetches it and opens it. Everything else a messenger has -- media and blobs, the rendezvous and
// contact cards, recovery, multi-device, EPH messages (ledger 185 and 186, deferred for alpha),
// read receipts, typing indicators -- is NOT HERE AND IS NOT STUBBED. Nothing in this package
// pretends to carry them: there is no method that accepts a picture and drops it.
//
// ---------------------------------------------------------------------------------------------
// DECISION S2-7: HOW A CLIENT REACHES THE SERVER. TAKEN, NOT RESOLVED.
// ---------------------------------------------------------------------------------------------
//
// The transport is INJECTED. [DeviceConfig.Transport] is an [sdk.MessageTransport] the caller
// built over a connect client the caller owns, and this package dials nothing, authenticates
// nothing and closes nothing.
//
// For the alpha the wiring taken is the one `msgrepo/cmd/message-server/stack_test.go` uses: two
// `connect.NewClient`s over `connect.NewNoContractClientOob` joined by two in-process routes, with
// no network space, no operator and no `ByJwt`.
//
// IT WAS TAKEN BECAUSE IT IS ONE OF EXACTLY TWO, AND THE OTHER IS BLOCKED ON AN ACTION NO CODE IN
// THIS WORKSPACE CAN PERFORM. `connect` HAS NO INBOUND LISTENER FOR CLIENT FRAMES. The query, so
// that the number is checkable rather than quoted: over connect at 27c50c2,
//
//	grep -rnE 'net\.Listen|websocket\.Upgrader' --include=*.go . | grep -v _test.go | grep -v /mls/
//
// answers SIX lines and not one of them accepts a peer's frames -- `egress.go:40` is a comment
// about a dialer Control callback, `extender/extender.go:149` and `:247` are the extender's own
// TCP and UDP ports, `ip_mux_upgrade.go:337` is the DNS/TCP mux, `transport.go:1423` is a UDP
// socket QUIC dials out of, and `tun.go:965` is gVisor's userspace stack inside the TUN. So a
// `connect.Client` receives a frame in exactly two ways: an in-process
// `connect.Route`, or a `connect.PlatformTransport` that DIALS OUT to `wss://connect.<host>` and
// is handed frames the platform routes to its client_id. `sdk/sim_device.go:115`'s authenticated
// `connect.NewClient(ByJwt, ApiUrl)` is the second, and it needs the message server to hold a
// `ByJwt` for a `network_client` that an admin OF THAT OPERATOR mints -- spec B §9.1 and §9.2 --
// which is an operator-admin action against a running URnetwork operator and is not something
// `connect`, `sdk` or the server module can do for itself.
//
// So the choice is not "loopback is faster". It is "loopback, or stand up an operator". The alpha
// takes loopback, and the day somebody has an operator credential the injection point does not
// move: [DeviceConfig.Transport] is already a transport over a client the caller built, and a
// platform-attached client goes in the same hole.
//
// WHAT THAT DOES NOT COVER, in full:
//
//   - The client is not authenticated to the server. `peer.Checks.ConnectionAuthenticated` is
//     satisfied by the connect layer's source id and nothing else; there is no account, no JWT and
//     no platform.
//   - There is no remote server. The two connect clients must be joined by a route somebody else
//     established, which today means one process -- the app and the server in one binary. A
//     message server on another machine is the platform-transport path above, and that is gated on
//     the credential, not on this package.
//   - An in-process server keeps its rows in `store.MemoryStore`, so the SERVER side of a
//     loopback wiring survives nothing. The CLIENT side now does: see the state store section
//     below.
//   - No NAT traversal, no TLS termination, no reconnection of the connect client itself, no
//     contract or provide mode. `NewNoContractClientOob` is what removes the contract requirement.
//   - Nothing here chooses a server id, discovers one, or verifies that the id it was handed is a
//     message server rather than any other connect peer.
//
// S2-7 is not resolved by any of this and this package makes no claim on it.
//
// WHAT HAS MOVED SINCE, AND IT IS HALF OF S2-7 AND NOT THE WHOLE OF IT. The platform-transport
// path above is now WRITTEN DOWN as [sdk.NewMessageClient]: a client strategy, an out-of-band
// control over the operator's api url, a `connect.Client` at the credential's own client_id, a
// `connect.PlatformTransport` dialling the operator's platform url, and the
// `SetProvideModesWithReturnTraffic` without which the platform delivers a message server's
// replies NOWHERE while every health signal still reads green. Until that existed it was eight
// lines inside `sdk/liveprobe`'s own main, so the C abi could reach nothing but an in-process
// loopback server; now the probe, the abi and anything else reach one declaration.
//
// NOTHING ABOUT THIS PACKAGE CHANGES FOR IT. [DeviceConfig.Transport] is still injected, this
// package still dials nothing and closes nothing, and the loopback wiring the alpha's cases run
// on is untouched. What is still open of S2-7 is the CREDENTIAL: a `network_client` ByJwt is
// minted by an admin of a running URnetwork operator (spec B sections 9.1 and 9.2), and nothing
// in connect, sdk or the server module mints one. AND THE PLATFORM PATH IS UNEXERCISED BY EVERY
// TEST IN THIS MODULE, for the same reason -- what the tests beside [sdk.NewMessageClient] hold
// is its shape, never that a frame crossed.
//
// ---------------------------------------------------------------------------------------------
// DECISION S2-2: WHAT HAPPENS ON A RECONNECT. RE-HELLO AND REBIND, AND NEVER SILENTLY.
// ---------------------------------------------------------------------------------------------
//
// A `GroupSession` MACs `write_auth` over the `server_nonce` it was constructed with, and §4.3.1
// replaces that nonce at every Hello. So a session outlives its nonce, and a record sealed against
// a dead one is refused on the wire. The alpha's answer has two halves because the problem has two
// halves, and the second is Wave 2's filed Finding E.
//
//  1. THE HALF THAT IS VISIBLE. [sdk.MessageTransport.NonceEpoch] moves at every completed Hello.
//     Every submit and every fetch in this package first compares that number against the one the
//     session was last bound at, and calls `RebindServerNonce` when it has moved. A rebind that
//     fails is returned to the caller as [ErrNonceRebind]; it is never swallowed and the send is
//     never reported as having happened.
//
//  2. THE HALF THAT IS NOT. NonceEpoch counts HELLOS, NOT CONNECTIONS. A connection replaced
//     underneath the binding without a Hello through it -- a second transport on the same connect
//     client, which is one client_id and therefore one connection to `peer.Connections` -- leaves
//     a superseded nonce readable at an unchanged number, and clause 1 sees nothing to repair. So
//     a record that comes back REASON_REJECTED gets exactly ONE recovery attempt: a fresh Hello
//     through this transport, a rebind onto the nonce it issued, `ReauthRecord` to re-MAC the
//     record that is already sealed -- which consumes no stream index and re-encrypts nothing --
//     and one resubmission. If that is refused too, [Group.Send] returns [ErrSubmitRefused] naming
//     both refusals. It NEVER returns nil.
//
// The retry is bounded at one and is not a loop. A second refusal is a fact about the group or the
// epoch rather than about the nonce, and a client that kept trying would turn a visible failure
// into a busy one.
//
// WHAT IS STILL OPEN. S2-2 is not resolved: this package observes a reconnect only through Hellos
// it performed and through refusals it was answered, and a `connect.Client` that reconnects
// underneath it announces nothing to either. The refusal-driven half is the alpha's cover for
// that, and it costs one wasted round trip per superseded nonce.
//
// ---------------------------------------------------------------------------------------------
// WHAT IS HANDED OVER OUT OF BAND, NAMED RATHER THAN LEFT FOR A READER TO FIND.
// ---------------------------------------------------------------------------------------------
//
// [Invite] carries four values from the founder to the joiner, by a channel this package does not
// have and does not invent -- the rendezvous and contact cards are out of scope. They are the
// three `connect/messagegroup`'s own join test names, plus the group id:
//
//   - the MLS Welcome and ratchet tree, which is ledger 44a's named hand-off;
//   - `pq_secret` AT THE EPOCH THE JOINER IS ADMITTED AT, drawn by [messagegroup.NewPqSecret].
//     THIS IS THE ONLY EPOCH THE INVITE CARRIES ONE FOR, and since ledger item 251 it is no
//     longer the only epoch there is: every later epoch draws its own and delivers it in that
//     epoch's device wrap (pqepoch.go), which is m1 task 14's carrier built. The Invite is
//     still the founding delivery -- MASTER section 7's own, out of band -- because the leaf a
//     joiner will occupy does not exist when the fan-out that opens its epoch is sealed;
//   - `group_handle_key`, computed by `GroupHandleKey(StorageRoot(...))`, whose carrier is M1-2;
//   - the 32 octet group id the server rows are keyed by.
//
// Every one of those is a value a PRODUCTION function produced. There is no test-only key source
// on the path: no constant key, no zero secret, no fixture exporter. What is missing is a
// CARRIER, and inventing one here would be inventing the rendezvous.
//
// ---------------------------------------------------------------------------------------------
// S2-14: THE MLS STATE STORE. THERE ARE TWO, AND WHICH ONE A DEVICE GETS IS THE CALLER'S CHOICE.
// ---------------------------------------------------------------------------------------------
//
// `connect/mls` publishes the `StateStore` INTERFACE and no implementation of it; the only one in
// the corpus was `messagegroup`'s own `memoryStateStore`, declared in a _test.go file and
// therefore unreachable from any build. This package ships both halves as production code:
//
//   - [NewMemoryStateStore] persists NOTHING and the name is the whole warning. Close the process
//     and every group is gone. It is still the DEFAULT when [DeviceConfig.StateStore] is nil,
//     because a device that silently started writing private keys into a directory the caller did
//     not choose would be a worse surprise than one that forgets.
//   - [OpenDurableStateStore] persists everything, on a directory, under a single-writer
//     exclusion, fsync'd before a value is observable. A device over one comes back into its
//     groups after a restart -- [Device.Restore] -- at the same epoch, under the same leaf, able
//     to open the other members' records sealed before the restart, to SHOW THE ONES IT SEALED
//     ITSELF from the copies it persisted before submitting them, and to seal new ones the other
//     side opens once it has reconciled. It shows its own from copies because since connect
//     4c030dc a member cannot open its own application record (connect messagegroup MG-4).
//
// WHAT PROTECTS THE PRIVATE KEYS IN THE DURABLE ONE: FILE PERMISSIONS AND NOTHING ELSE. Every
// octet is written in the clear -- the MLS epoch state with this member's leaf private key and
// path-secret ladder in it, every key package's private halves, this device's Ed25519 identity,
// and each group's `pq_secret` and `group_handle_key`. No passphrase, no key derivation, no
// keychain. The directory is 0o700 and the files 0o600, which is a real bound on POSIX and is not
// a bound this code sets on Windows. **S2-24 is what must rule that**, and until it is ruled the
// sentence above is the whole answer. [DurableStateStore]'s own header states it at length and
// says what closing it would need.
//
// THE OTHER DURABLE HALF IS THE STREAM INDEX RESERVER, which is [sdk.StreamStore], and the
// asymmetry that used to exist between the two is gone: the reserver is crash safe because a
// reused stream index is a reused nonce under a reused record key, and the state store now follows
// the same discipline because a lost MLS group is a lost conversation rather than an
// inconvenience.
//
// WHERE EACH HALF IS MEASURED, AND THIS SENTENCE IS A CORRECTION. It used to say "`sdk/cp3b`'s
// restart case measures both across a process boundary at once", and that was FALSE: cp3b's
// restart closes both stores and reopens them INSIDE ONE TEST PROCESS, and no `os/exec` existed
// anywhere in this package or in cp3b. The two measurements are now in two places and each says
// what it is:
//
//   - `sdk/cp3b`'s restart case is the WHOLE SEAM -- a real message server, real submissions, real
//     fetches -- across an in-process restart in which everything held in memory is dropped and
//     the only thing that crosses is two directories on the disk.
//   - `urmessage/crossprocess_test.go` is a REAL PROCESS BOUNDARY: it re-executes the test binary,
//     and a process that did not exist when the records were sealed re-derives the same MLS
//     exporter, comes back at the same sender_handle, opens a record sealed before the death, and
//     seals again at the index after the dead process's last one. There is no server in it, on
//     purpose -- the server holds no key material, so the property is entirely between the disk
//     and the key schedule, and a second process cannot reach an in-process server in any case.
//
// ---------------------------------------------------------------------------------------------
// WHAT A DURABLE IDENTITY COSTS: ONE COPY OF THE APP-DATA FOLDER IS TWO DEVICES ON ONE IDENTITY.
// ---------------------------------------------------------------------------------------------
//
// Persisting the identity is what makes a restart a RESTORE, and it is also what makes a COPY
// dangerous. Before the store existed a restarted device drew a fresh key and a fresh group, so a
// copied directory was harmless. Now a copied folder is a second device at the same leaf, the same
// sender_handle and the same stream counter -- and two records under one
// (epoch, sender_handle, stream_index) are one record_key and one nonce, which spec A §5.6 calls a
// total break of both AEADs for that record. The single-writer exclusion does not reach it: it is
// held per DIRECTORY and a copy is a second directory.
//
// SO A RESTORED GROUP WILL NOT SEAL UNTIL IT HAS LISTENED, AND IT WILL NOT GO ON SEALING ONCE THE
// SERVER HAS TOLD IT WHY. [Group.Receive] holds the stream indices it finds on the server against
// the durable reserver's own high water -- over a walk that was COMPLETE AND CLEAN, never over one
// the server said was short or one that lost a record -- and a group that finds an index its
// reserver never allocated refuses to seal for the life of the process with [ErrIdentityInUse].
// A §4.5 REASON_STREAM_INDEX_REUSED at the SUBMIT is the same refusal, which is what bounds a copy
// whose user keeps typing without ever fetching.
//
// [Device.Restore] states exactly what that covers -- every copy that is behind the original, and
// every copy that cannot reach the server at all -- and what it does not, WITH THE BOUND ON THE
// RESIDUAL AND THE BOUND'S PRECONDITION: for a copy whose submissions are ANSWERED, two copies that
// are exactly level both seal at one index before either can see the other and the cost is ONE
// CONTESTED INDEX PER GROUP PER PROCESS LIFETIME of the losing copy. A copy that reconciled and
// THEN went dark is bounded by nothing at all, because every clause of the check is fed by
// something the server said. Both are measured rather than asserted, in `sdk/cp3b`. Closing either
// needs a new leaf for the copy, which is an MLS Update commit -- reachable at the handle now that
// J1-8 is closed, and reachable from nothing in this package's own API, which is why it is still
// filed as S2-28.
//
// WHAT WAS OPEN HERE AND IS NOT ANY MORE. `messagegroup.GroupEngine` used to declare four methods,
// none of which opened a persisted group, so this package carried its own
// `messagegroup.GroupHandle` over `mls.LoadGroup` to make the restore reachable at all -- a second
// implementation of twenty six methods, two of which (`Process` and `ApplyCommit`) it could not
// write at all, because `messagegroup.EngineProcessed` holds its staged commit in an unexported
// field. A restored group therefore refused to ingest a commit. That was J1-8. `GroupEngine` grew a
// fifth method, `LoadGroup(groupId []byte, epoch uint64) (GroupHandle, error)`, and this package's
// copy was DELETED: a restored group is now the same handle type a founded or a joined one is, and
// TestRestoredGroupIngestsACommitAndEntersEpochTwo drives the pair that used to refuse.
package urmessage
