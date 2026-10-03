package main

/*
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>
#include "callbacks_message.h"
*/
import "C"

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/sdk/urmessage"
)

// THE MESSAGING C ABI, AND IT IS HAND-WRITTEN ON PURPOSE.
//
// exports_gen.go is produced by gen/gen.go from package sdk's own surface, under curated
// classification lists that say which types are behavioural and which are data. The messaging
// surface is NOT in package sdk: it is sdk/urmessage, plus three constructors in sdk that the
// generator's walk does not reach, plus one interface that belongs to connect/messagegroup.
// Widening the generator to a second and third package is a change to the classification model
// and would reflow every one of the 620 existing declarations in the same commit. That is a
// separate, larger job. So this file is the messaging surface written by hand, in the
// generator's own style, ALONGSIDE the generated file rather than inside it -- the same reason
// exports_manual.go gives for the byte-buffer exports, and it is not repeated there.
//
// TWO CONSEQUENCES OF BEING HAND-WRITTEN, NAMED RATHER THAN LEFT TO BE DISCOVERED:
//   - include/urnetwork_message.h is the header for this file and is hand-written too. The
//     generated include/urnetwork_sdk.h does not declare these; the two headers are independent
//     and both ship. The cgo-generated header emitted next to the library declares both.
//   - include/urnetwork_sdk.def names every export in this file because gen.go's manualExports()
//     picks them up. It used to be STALE -- 0 of 34 names, so an MSVC consumer linking through the
//     import library found none of the messaging surface -- and it is now held by
//     gen.TestTheDefNamesEveryHandWrittenExportThatShips rather than by this sentence.
//
// ── WHAT IS EXPOSED IS WHAT IS PROVEN, AND THE SILENCES ARE DELIBERATE ───────────────────────
//
// THIS PARAGRAPH USED TO READ "there is no receipt, no reaction, no reply, no edit, no delete and
// no media", AND HALF OF IT IS NO LONGER TRUE. The content envelope landed (urmessage/kind.go, the
// 2026-09-17 ruling) and urmessage now carries a kind, a gap reason, a reply parent, a tombstone
// flag and a reaction list on every Message. The READ side of those five crosses here -- see
// messageInfo and the two reaction accessors at the bottom of this file -- because a caller that
// cannot tell a GAP from a message with no text is rendering a conversation it has been told
// nothing about (msgrepo ledger item 236).
//
// AND THE SEND SIDE OF FOUR OF THEM NOW CROSSES TOO, WHICH IS WHAT THIS PARAGRAPH USED TO SAY WAS
// OWED. It read "urmessage.Group has SendReply, React, Unreact and Delete and NO export here calls
// them: a C caller can render a reply, a reaction and a tombstone and cannot make one." That is no
// longer true: urnet_message_group_send_reply, _react, _unreact and _delete are shipping exports in
// the group section below, and a C caller PARTICIPATES in a conversation rather than only watching
// one. What is still read-only is the pair urmessage itself does not carry -- an EDIT and a receipt
// -- and those are absent here because they are absent there.
//
// A body is still opaque octets, in and out: nothing here reads one, and the kind says under which
// grammar it was read rather than what it contains. There is still no receipt, no edit and no
// media, not as a stub and not as an export that answers a plausible empty result, because
// urmessage does not carry them.
//
// AND THE ROLE MODEL'S SURFACE CROSSES (MASTER §11, ledger item 242's R3): the roster with each
// member's role, this device's own role, and the two policy verbs -- set_role and
// transfer_ownership -- with the commit's outcome projected as a KIND a caller can branch on, so
// that "your role does not permit this", "somebody else's commit landed first, fetch and retry"
// and "the network failed" are three different sentences on the far side of the wall rather than
// one malloc'd string. See the roster section below.
//
// ── DECISION: BLOCKING, ON THE CALLER'S OWN THREAD, WITH A CANCEL HANDLE ─────────────────────
//
// Connect, Open, Send and Receive block and take a context. They are exported as BLOCKING calls
// the caller runs on a thread of its own, NOT as callback-completions, and the argument has four
// parts:
//
//  1. There is no completion model to map onto. urmessage has no queue, no dispatcher and no
//     receive push -- sdk/message_transport.go says it in its own voice: "there is no server
//     push, and the receive path is a poll". An async ABI would have to INVENT the completion
//     machinery, which would put the concurrency model in the binding rather than in the code
//     that is measured.
//  2. The caller already has threads and its own idea of where work belongs. A WinUI3 app awaits
//     on winrt::resume_background(); a blocking call is exactly what such a coroutine wants, and
//     a completion delivered on a Go-owned thread is exactly what it does not -- it would arrive
//     with no apartment and have to be marshalled back across.
//  3. It is what the Windows app's own shape needs. urmsg::demo::GetWorld() is called from
//     CollectDiagnostics() at main.cpp:168, BEFORE winrt::init_apartment() at main.cpp:183, and
//     DemoWorld.h states that a winrt type there would be constructed without an apartment.
//     Every function in this file is plain C over uint64_t handles and touches no COM, so a
//     world can be constructed on that thread for nothing and the network work happens later,
//     elsewhere.
//  4. A blocking call needs a way to be woken, and that is the part a "just block" ABI forgets.
//     Device.Connect rides out a MEASURED ~60s operator window on a budget that defaults to 90
//     SECONDS. An app that is closing cannot wait that out. So every blocking export takes a
//     urnet_message_context handle: urnet_message_context_cancel wakes it from any thread and
//     the call returns with the context's error instead of spending its budget. Pass 0 for an
//     uncancellable call.
//
// WHAT THIS DOES NOT DO: it does not make a receive arrive on its own. A caller polls
// urnet_message_group_receive on its own schedule, because a poll is what the transport is.
//
// ── DECISION: A BODY CROSSES AS COUNTED OCTETS, NEVER AS char* AND NEVER INSIDE JSON ─────────
//
// urmessage.Group.Send takes a Go string and Message.Text is a Go string, and A GO STRING IS NOT
// TEXT: a body is octets that arrive from another device. Since the content envelope landed
// (urmessage/kind.go, the 2026-09-17 ruling) a TEXT and a REPLY tail IS checked for valid UTF-8 on
// the way in and on the way out -- but valid UTF-8 is NOT NUL-free, U+0000 being one octet of it,
// and a kind this build does not know carries no text at all. So:
//
//   - A body is (const uint8_t*, int32_t) going in and the buffer-out pattern coming back. It is
//     never a char*: a char* would truncate a body at its first 0x00 octet and hand back a
//     SHORTER message with no error raised anywhere.
//   - A body is never a field of a json result. encoding/json replaces every invalid UTF-8 byte
//     with U+FFFD SILENTLY, so a json-carried body is a body the binding corrupted. That is the
//     whole reason urnet_message_list_info and urnet_message_list_body are two calls: the
//     metadata is json, the body is octets, and they do not mix.
//
// The caller decides whether its bodies are UTF-8. This ABI does not and cannot, because the
// envelope is unruled.
//
// ── LIFETIME ────────────────────────────────────────────────────────────────────────────────
//
// Every uint64_t this file returns is a handle in handles.go's registry and keeps its Go object
// reachable until urnet_release. Releasing does not close: call the object's own close first
// where one exists (a context, a store, a transport, a device, a group). Zero is never a handle
// -- it is "none" on the way in and "failure" or "empty" on the way out.
//
// urnet_live_handle_count is the measurement, and ctest/message_abi_test.c holds it across a
// whole conversation.

// ── the cancel handle ───────────────────────────────────────────────────────────────────────

// messageContext is a context.Context a C caller can hold and cancel from another thread. It is
// the piece that makes a blocking ABI closeable; see the threading decision above.
type messageContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

//export urnet_message_context_new
func urnet_message_context_new() C.uint64_t {
	defer cgoGuard("urnet_message_context_new")
	ctx, cancel := context.WithCancel(context.Background())
	return C.uint64_t(newHandle(&messageContext{ctx: ctx, cancel: cancel}))
}

// urnet_message_context_cancel wakes every blocking call holding this context. It is safe from
// any thread and it is idempotent. It is the "stop" half of the abi contract's
// stop-then-release: urnet_release alone leaves the context uncancelled.
//
//export urnet_message_context_cancel
func urnet_message_context_cancel(self C.uint64_t) {
	defer cgoGuard("urnet_message_context_cancel")
	self_, ok := resolveHandle[*messageContext](uint64(self), "urnet_message_context_cancel")
	if !ok || self_ == nil {
		return
	}
	self_.cancel()
}

// messageCtx resolves a context handle. A zero handle is context.Background(), which is an
// UNCANCELLABLE call and is what a caller that passes nothing asked for; an unknown handle is
// REFUSED rather than quietly downgraded to Background, because a call that cannot be cancelled
// while its caller believes it can is the exact failure this handle exists to prevent.
func messageCtx(id C.uint64_t, name string) (context.Context, bool) {
	if id == 0 {
		return context.Background(), true
	}
	self_, ok := resolveHandle[*messageContext](uint64(id), name)
	if !ok || self_ == nil {
		return nil, false
	}
	return self_.ctx, true
}

// ── the two durable stores, and the stream index reserver over one of them ──────────────────

//export urnet_message_stream_store_open
func urnet_message_stream_store_open(dir *C.char, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_stream_store_open")
	store, err := sdk.OpenStreamStore(goString(dir))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(store))
}

//export urnet_message_stream_store_close
func urnet_message_stream_store_close(self C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_message_stream_store_close")
	self_, ok := resolveHandle[*sdk.StreamStore](uint64(self), "urnet_message_stream_store_close")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	if err := self_.Close(); err != nil {
		setErrorOut(outError, err)
		return C.bool(false)
	}
	return C.bool(true)
}

//export urnet_message_stream_index_reserver_new
func urnet_message_stream_index_reserver_new(streamStore C.uint64_t) C.uint64_t {
	defer cgoGuard("urnet_message_stream_index_reserver_new")
	store, ok := resolveHandle[*sdk.StreamStore](uint64(streamStore), "urnet_message_stream_index_reserver_new")
	if !ok || store == nil {
		return 0
	}
	return C.uint64_t(newHandle(sdk.NewStreamIndexReserver(store)))
}

//export urnet_message_durable_state_store_open
func urnet_message_durable_state_store_open(dir *C.char, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_durable_state_store_open")
	store, err := urmessage.OpenDurableStateStore(goString(dir))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(store))
}

//export urnet_message_durable_state_store_close
func urnet_message_durable_state_store_close(self C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_message_durable_state_store_close")
	self_, ok := resolveHandle[*urmessage.DurableStateStore](uint64(self), "urnet_message_durable_state_store_close")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	if err := self_.Close(); err != nil {
		setErrorOut(outError, err)
		return C.bool(false)
	}
	return C.bool(true)
}

// ── the platform-attached client, which is what makes the rest of this abi reach a real server ──

// urnet_message_client_new builds a connect client ATTACHED TO THE URNETWORK PLATFORM: it dials
// wss://connect.<host> with the operator-minted by_client_jwt, and it is what
// urnet_message_transport_new's client parameter takes.
//
// IT IS THE EXPORT THAT USED NOT TO EXIST, and until it did, a C caller could reach an in-process
// loopback server and nothing else -- not because the binding was incomplete but because S2-7's
// second half was open. It does not close S2-7's FIRST half: the by_client_jwt is minted by an
// admin of a running URnetwork operator (spec B §9.1) and nothing in this abi mints, fetches or
// validates one. This takes the credential the caller already holds.
//
// THIS PATH HAS NEVER BEEN RUN AGAINST A REAL OPERATOR FROM THIS ABI, and the module says so in
// sdk/message_client.go's own header rather than only here. What is held by tests is the SHAPE:
// which arguments are refused, what the urls derive to, that the client carries the client_id the
// credential names, and that the provide modes are set. Not that a frame ever crossed.
//
// ARGUMENTS. by_client_jwt is required. host is the operator host name, e.g. "ur.io"; the two
// service urls are DERIVED from it the way the rest of sdk derives them, so env "" or "main"
// gives wss://connect.<host> and any other env gives wss://<env>-connect.<host>. env may be NULL
// or "" for the deployed one. instance_id may be NULL or "" to draw a fresh one; pass the uuid
// you kept to reconnect as the same installation. app_version may be NULL for this build's
// default. Every refusal answers 0 AND sets out_error.
//
// THE RETURNED HANDLE OWNS A LIVE CONNECTION. Call urnet_message_client_close before urnet_release
// -- release alone leaves the websocket, the platform transport's reconnect loop and the client's
// goroutines running for the life of the process. Close the transport and the device FIRST: they
// are built over this and they do not close it.
//
// IT DOES NOT BLOCK AND IT DOES NOT REPORT WHETHER THE CREDENTIAL WAS ACCEPTED. The dial happens
// on a goroutine and reconnects on its own; the first call that finds out is
// urnet_message_device_connect, and a client_id that has just re-dialled is not routed to for
// about sixty seconds.
//
//export urnet_message_client_new
func urnet_message_client_new(byClientJwt *C.char, host *C.char, env *C.char, instanceId *C.char, appVersion *C.char, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_client_new")
	config := &sdk.MessageClientConfig{
		ByClientJwt: goString(byClientJwt),
		Host:        goString(host),
		Env:         goString(env),
		AppVersion:  goString(appVersion),
	}
	// AN EMPTY instance_id IS "DRAW ONE" AND A MALFORMED ONE IS A REFUSAL, which is not the
	// same thing and is the reason this is not one ParseId call with the error swallowed. A
	// caller that meant to reconnect as a kept installation and mistyped the uuid would
	// otherwise silently become a NEW installation, which is exactly the case the id exists to
	// distinguish.
	if raw := goString(instanceId); raw != "" {
		parsed, err := connect.ParseId(raw)
		if err != nil {
			setErrorOut(outError, fmt.Errorf("urnet_message_client_new: instance_id %q is not a uuid; pass NULL or \"\" to draw a fresh one: %w", raw, err))
			return 0
		}
		config.InstanceId = parsed
	}
	client, err := sdk.NewMessageClient(context.Background(), config)
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(client))
}

// urnet_message_client_id is the client_id the credential named, as a uuid string. It is the
// identity the platform routes to, and a caller that wants to know WHICH client this is -- for a
// log line, or to compare against what an operator console shows -- gets it here rather than by
// parsing the jwt a second time. Free with urnet_free_string.
//
//export urnet_message_client_id
func urnet_message_client_id(self C.uint64_t) *C.char {
	defer cgoGuard("urnet_message_client_id")
	self_, ok := resolveHandle[*sdk.MessageClient](uint64(self), "urnet_message_client_id")
	if !ok || self_ == nil {
		return nil
	}
	return cString(self_.ClientId().String())
}

// urnet_message_client_platform_url is the url this client actually dialled. It exists because
// the derivation from host and env happens inside the module, so it is the one thing about this
// client a caller cannot otherwise check -- and dialling the production authority from a staging
// env is a mistake that looks exactly like working. Free with urnet_free_string.
//
//export urnet_message_client_platform_url
func urnet_message_client_platform_url(self C.uint64_t) *C.char {
	defer cgoGuard("urnet_message_client_platform_url")
	self_, ok := resolveHandle[*sdk.MessageClient](uint64(self), "urnet_message_client_platform_url")
	if !ok || self_ == nil {
		return nil
	}
	return cString(self_.PlatformUrl())
}

// urnet_message_client_close stops the platform transport, the client and the context under them.
// It is idempotent. Everything built OVER this client -- the transport, the device, the groups --
// should be closed first; none of them closes this.
//
//export urnet_message_client_close
func urnet_message_client_close(self C.uint64_t) {
	defer cgoGuard("urnet_message_client_close")
	self_, ok := resolveHandle[*sdk.MessageClient](uint64(self), "urnet_message_client_close")
	if !ok || self_ == nil {
		return
	}
	self_.Close()
}

// ── §4.3.5 push ─────────────────────────────────────────────────────────────────────────────

// urnet_message_group_ensure_subscribed subscribes this group to the server's push when it holds
// no current subscription (none, an older epoch, or a connection a Hello has replaced). Answers 1
// when it subscribed now, 0 when the subscription was already current, and -1 with out_error when
// the server refused. After a 1, receive once: the subscription only announces what arrives later.
//
//export urnet_message_group_ensure_subscribed
func urnet_message_group_ensure_subscribed(self C.uint64_t, ctx C.uint64_t, outError **C.char) C.int32_t {
	defer cgoGuard("urnet_message_group_ensure_subscribed")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_ensure_subscribed")
	if !ok || self_ == nil {
		return -1
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_ensure_subscribed")
	if !ok {
		return -1
	}
	subscribed, err := self_.EnsureSubscribed(ctx_)
	if err != nil {
		setErrorOut(outError, err)
		return -1
	}
	if subscribed {
		return 1
	}
	return 0
}

// urnet_message_device_wait_push waits up to timeout_ms for a §4.3.5 push and answers the group id
// it names as hex, or NULL on timeout. A push carries no records: answer it with
// urnet_message_group_receive. Free with urnet_free_string.
//
//export urnet_message_device_wait_push
func urnet_message_device_wait_push(self C.uint64_t, timeoutMs C.int64_t) *C.char {
	defer cgoGuard("urnet_message_device_wait_push")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_wait_push")
	if !ok || self_ == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	groupId, err := self_.WaitPush(ctx)
	if err != nil {
		return nil
	}
	return cString(hex.EncodeToString(groupId))
}

// ── the route client: the server's own endpoint, through a URnetwork exit or directly ───────

// urnet_message_route_client_new reaches a message server at its OWN TLS endpoint rather than
// through the operator (sdk/message_route.go, ledger 268). The handle satisfies the same seam as
// urnet_message_client_new's, so urnet_message_transport_new takes it unchanged.
//
// mode is URNET_MESSAGE_ROUTE_URNETWORK (0, the default): the connection leaves through a
// URnetwork exit provider over a tunnel inside this process -- no adapter, no service, no
// administrator -- and the server sees the exit's address. Or URNET_MESSAGE_ROUTE_DIRECT (1): an
// ordinary connection, which shows this device's address to the message server.
//
// endpoint is a wss:// url, e.g. "wss://74.50.11.53/urmessage/v1". pin is the SHA-256 of the
// server certificate's SubjectPublicKeyInfo as 64 hex characters, optionally "sha256/"-prefixed;
// a server presenting any other key is never sent a frame. by_client_jwt and host are needed for
// URNET_MESSAGE_ROUTE_URNETWORK, where the tunnel's window clients are minted from the credential
// on that operator, and are ignored for DIRECT. by_client_jwt IS A SECRET. env and app_version may
// be NULL. Every refusal answers 0 AND sets out_error.
//
// IT DOES NOT BLOCK. The tunnel finds its exit providers and the session dials on their own
// goroutines and redial with backoff; urnet_message_device_connect is what finds out whether the
// server answered, and urnet_message_route_client_status says how the route is doing.
//
// CLOSE IT WITH urnet_message_route_client_close BEFORE urnet_release, after the transport and the
// device built over it.
//
//export urnet_message_route_client_new
func urnet_message_route_client_new(byClientJwt *C.char, host *C.char, env *C.char, endpoint *C.char, pin *C.char, mode C.int32_t, appVersion *C.char, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_route_client_new")
	parsedPin, err := sdk.ParseMessageRoutePin(goString(pin))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	client, err := sdk.NewMessageRouteClient(context.Background(), &sdk.MessageRouteConfig{
		Endpoint:    goString(endpoint),
		Pin:         parsedPin,
		Mode:        sdk.MessageRouteMode(mode),
		ByClientJwt: goString(byClientJwt),
		Host:        goString(host),
		Env:         goString(env),
		AppVersion:  goString(appVersion),
	})
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(client))
}

// urnet_message_route_client_status is the route's state as json: {"mode":"urnetwork"|"direct",
// "connected":bool, "connects":n, "last_error":"...", "window_providers":n,
// "window_countries":[...]}. connects above 1 means the session has been re-established. The
// window fields are the tunnel's exit providers and are zero in direct mode. Free with
// urnet_free_string.
//
//export urnet_message_route_client_status
func urnet_message_route_client_status(self C.uint64_t) *C.char {
	defer cgoGuard("urnet_message_route_client_status")
	self_, ok := resolveHandle[*sdk.MessageRouteClient](uint64(self), "urnet_message_route_client_status")
	if !ok || self_ == nil {
		return nil
	}
	encoded, err := json.Marshal(self_.Status())
	if err != nil {
		return nil
	}
	return cString(string(encoded))
}

// urnet_message_route_client_close ends the session and the tunnel. Idempotent. Close the device
// and the transport over it first; neither closes this.
//
//export urnet_message_route_client_close
func urnet_message_route_client_close(self C.uint64_t) {
	defer cgoGuard("urnet_message_route_client_close")
	self_, ok := resolveHandle[*sdk.MessageRouteClient](uint64(self), "urnet_message_route_client_close")
	if !ok || self_ == nil {
		return
	}
	self_.Close()
}

// ── the transport, over a connect client this abi does not produce ──────────────────────────

// urnet_message_transport_new binds §10.1 to one message server over a connect client the CALLER
// owns: nothing here dials, authenticates or closes that client.
//
// PROTOCOL_VERSION IS URNET_MESSAGE_PROTOCOL_VERSION, OR 0 FOR IT, AND NOTHING ELSE. It used to be
// passed straight through with no value documented anywhere, and 0 was the trap: every other
// numeric parameter in this abi uses 0 for "the default", and a caller who passed 0 by that analogy
// got a transport that offered no version at all, which the server answers two calls later, at
// Hello, as REASON_UNSUPPORTED_VERSION. A review built a whole conversation that way and lost every
// check after it. So 0 now takes the version this build speaks, like every other 0 here, and any
// other value is refused HERE, by name, before it can become a Hello the server refuses.
//
// WHERE THE CLIENT HANDLE COMES FROM, WHICH IS A SENTENCE THAT CHANGED. A connect.Client receives
// a frame in exactly two ways -- an in-process connect.Route, or a connect.PlatformTransport that
// dials wss://connect.<host> with an operator-minted ByJwt for a network_client (spec B §9.1).
// This abi now produces the SECOND: urnet_message_client_new, above. The first is the loopback
// harness, which is behind a build tag and ships in nothing.
//
// UNTIL THAT EXPORT EXISTED THERE WAS NO SOURCE AT ALL, and this paragraph said so. What is still
// open of S2-7 is the CREDENTIAL and only the credential: minting a network_client ByJwt is an
// operator-admin action against a running URnetwork operator, and nothing in connect, sdk or this
// binding does it. The client no longer has to be invented; the token still has to be handed in.
//
//export urnet_message_transport_new
func urnet_message_transport_new(client C.uint64_t, serverClientId *C.char, protocolVersion C.uint32_t, timeoutMs C.int64_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_transport_new")
	client_, ok := resolveHandle[sdk.MessageTransportClient](uint64(client), "urnet_message_transport_new")
	if !ok {
		return 0
	}
	version, err := messageProtocolVersionOf(uint32(protocolVersion))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	server, err := connect.ParseId(goString(serverClientId))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client:          client_,
		Server:          server,
		ProtocolVersion: version,
		Timeout:         time.Duration(timeoutMs) * time.Millisecond,
	})
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(transport))
}

// messageProtocolVersion is the one §4.3.1 protocol version this build speaks, and it is
// URNET_MESSAGE_PROTOCOL_VERSION in include/urnetwork_message.h. The server this alpha is deployed
// from declares the same number, as `const protocolVersion = 1` in msgrepo
// cmd/message-server/server.go, and answers anything else REASON_UNSUPPORTED_VERSION.
const messageProtocolVersion uint32 = 1

// messageProtocolVersionOf is protocol_version as the abi takes it: 0 is this build's version, the
// build's version is itself, and every other value is refused with a sentence naming the one that
// works.
func messageProtocolVersionOf(protocolVersion uint32) (uint32, error) {
	switch protocolVersion {
	case 0, messageProtocolVersion:
		return messageProtocolVersion, nil
	}
	return 0, fmt.Errorf("urnet_message_transport_new: protocol_version %d is not a version this build speaks; pass %d (URNET_MESSAGE_PROTOCOL_VERSION), or 0 for it",
		protocolVersion, messageProtocolVersion)
}

// urnet_message_transport_close stops receiving. The connect client under it is the caller's and
// is NOT closed.
//
//export urnet_message_transport_close
func urnet_message_transport_close(self C.uint64_t) {
	defer cgoGuard("urnet_message_transport_close")
	self_, ok := resolveHandle[*sdk.MessageTransport](uint64(self), "urnet_message_transport_close")
	if !ok || self_ == nil {
		return
	}
	self_.Close()
}

// ── the device ──────────────────────────────────────────────────────────────────────────────

// cAdapterMessageConnectAttempt carries ConnectPolicy.OnAttempt across to C. It holds a C
// function pointer and a C user_data and no Go pointer, which is what cgo's pointer rules
// require of a value a Go struct keeps; the generated adapters in exports_gen.go have the same
// shape and this one follows them.
type cAdapterMessageConnectAttempt struct {
	cb       C.urnet_message_connect_attempt_cb
	userData unsafe.Pointer
}

func (self *cAdapterMessageConnectAttempt) onAttempt(attempt urmessage.ConnectAttempt) {
	defer cgoGuard("urnet_message_connect_attempt_cb")
	var err *C.char
	if attempt.Err != nil {
		err = cString(attempt.Err.Error())
	}
	C.urnet_invoke_message_connect_attempt(self.cb, self.userData,
		C.int32_t(attempt.Attempt),
		C.int64_t(attempt.Elapsed.Milliseconds()),
		C.int64_t(attempt.Backoff.Milliseconds()),
		err)
	if err != nil {
		cStringFree(err)
	}
}

// urnet_message_device_new builds one device over a transport, a reserver and, optionally, a
// durable state store.
//
// state_store may be 0, which takes urmessage's in-memory store: it persists NOTHING and every
// group is gone when the process ends. That is urmessage's own default and its reason is that a
// device which silently wrote private keys into a directory the caller did not choose would be
// the worse surprise. Pass a urnet_message_durable_state_store_open handle to persist.
//
// connect_budget_ms and connect_attempt_timeout_ms are 0 for urmessage's defaults (90s and 10s).
// THE BUDGET IS THE BOUND ON HOW LONG urnet_message_device_connect BLOCKS, whatever the attempt
// timeout: an attempt is cut to what is left of the budget, so a budget under one attempt is one
// attempt of the budget's length. It used to be checked only after an attempt returned, and a
// 500 ms budget blocked for the whole 10 s default attempt.
// connect_attempt_cb may be NULL; when it is not it fires for every Hello that did not connect,
// on the thread inside urnet_message_device_connect, which is how a caller says "Reconnecting..."
// DURING the ~60s operator window rather than after it.
//
//export urnet_message_device_new
func urnet_message_device_new(transport C.uint64_t, reserver C.uint64_t, stateStore C.uint64_t, connectBudgetMs C.int64_t, connectAttemptTimeoutMs C.int64_t, connectAttemptCb C.urnet_message_connect_attempt_cb, connectAttemptUserData unsafe.Pointer, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_device_new")
	transport_, ok := resolveHandle[*sdk.MessageTransport](uint64(transport), "urnet_message_device_new")
	if !ok {
		return 0
	}
	reserver_, ok := resolveHandle[messagegroup.StreamIndexReserver](uint64(reserver), "urnet_message_device_new")
	if !ok {
		return 0
	}
	config := urmessage.DeviceConfig{
		Transport: transport_,
		Reserver:  reserver_,
		Connect: urmessage.ConnectPolicy{
			Budget:         time.Duration(connectBudgetMs) * time.Millisecond,
			AttemptTimeout: time.Duration(connectAttemptTimeoutMs) * time.Millisecond,
		},
	}
	// NOT an unconditional `config.StateStore = stateStore_`. StateStore is an INTERFACE field,
	// and a nil *DurableStateStore assigned into it is a NON-NIL interface holding a nil
	// pointer -- which is not the nil that urmessage tests for when it decides to take the
	// memory store, and which panics on the first call to it instead.
	if stateStore != 0 {
		stateStore_, ok := resolveHandle[*urmessage.DurableStateStore](uint64(stateStore), "urnet_message_device_new")
		if !ok || stateStore_ == nil {
			return 0
		}
		config.StateStore = stateStore_
	}
	if connectAttemptCb != nil {
		adapter := &cAdapterMessageConnectAttempt{cb: connectAttemptCb, userData: connectAttemptUserData}
		config.Connect.OnAttempt = adapter.onAttempt
	}
	device, err := urmessage.NewDevice(config)
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(device))
}

// urnet_message_device_connect says §4.3.1's Hello and BLOCKS until it is answered, until the
// budget is spent, or until ctx is cancelled. See the threading decision at the top of this
// file: the budget defaults to 90 seconds and a ui thread must not be what waits it out.
//
// A budget spent on silence is urmessage's ErrReconnecting, which means "not yet, ask again" and
// NOT "failed": on the deployed server a reconnecting client_id is not routed to for about sixty
// seconds. A caller that shows a user "could not connect" here is telling them something false.
//
//export urnet_message_device_connect
func urnet_message_device_connect(self C.uint64_t, ctx C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_message_device_connect")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_connect")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_device_connect")
	if !ok {
		return C.bool(false)
	}
	if err := self_.Connect(ctx_); err != nil {
		setErrorOut(outError, err)
		return C.bool(false)
	}
	return C.bool(true)
}

//export urnet_message_device_close
func urnet_message_device_close(self C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_message_device_close")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_close")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	if err := self_.Close(); err != nil {
		setErrorOut(outError, err)
		return C.bool(false)
	}
	return C.bool(true)
}

// urnet_message_device_key_package is the buffer-out pattern (see copyOut in exports_manual.go):
// call once with out == NULL to learn the size, again with a buffer that large to fill it.
//
//export urnet_message_device_key_package
func urnet_message_device_key_package(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_message_device_key_package")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_key_package")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	keyPackage, err := self_.KeyPackage()
	if err != nil {
		setErrorOut(outError, err)
		if inoutLen != nil {
			*inoutLen = 0
		}
		return C.bool(false)
	}
	return copyOut(out, inoutLen, keyPackage)
}

//export urnet_message_device_groups
func urnet_message_device_groups(self C.uint64_t) C.uint64_t {
	defer cgoGuard("urnet_message_device_groups")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_groups")
	if !ok || self_ == nil {
		return 0
	}
	return C.uint64_t(newGroupList(self_.Groups()))
}

// urnet_message_device_restore rebuilds every group a DURABLE state store holds. A restored
// group will not seal until urnet_message_group_receive has run once over it -- that is
// urmessage's ErrNotReconciled, and it is what bounds a copied app-data folder.
//
// Like receive, a non-zero list and a non-NULL out_error can both come back: a restore that
// rebuilt some groups and failed on others carries both halves.
//
//export urnet_message_device_restore
func urnet_message_device_restore(self C.uint64_t, ctx C.uint64_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_device_restore")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_restore")
	if !ok || self_ == nil {
		return 0
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_device_restore")
	if !ok {
		return 0
	}
	groups, err := self_.Restore(ctx_)
	if err != nil {
		setErrorOut(outError, err)
	}
	return C.uint64_t(newGroupList(groups))
}

//export urnet_message_device_create_group
func urnet_message_device_create_group(self C.uint64_t, ctx C.uint64_t, groupId *C.uint8_t, groupIdLen C.int32_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_device_create_group")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_create_group")
	if !ok || self_ == nil {
		return 0
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_device_create_group")
	if !ok {
		return 0
	}
	group, err := self_.CreateGroup(ctx_, goBytes(groupId, groupIdLen))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(group))
}

// urnet_message_device_forget_group is "delete for me and leave": urmessage's Device.ForgetGroup.
// It CLOSES the group and ERASES this device's whole copy of it -- every epoch key, every copy of a
// line this device sent, the group's record -- and commits nothing, so nobody is told and this
// device's leaf stays in the group until somebody removes it with urnet_message_group_remove_member
// (msgrepo ledger 257, ruling 48: a leave is product surface, not an MLS proposal).
//
// IT ANSWERS A URNET_MESSAGE_FORGET_* KIND AND NOT A BOOL, because a caller has to tell apart two
// failures a bool folds together (msgrepo ledger §7, 2026-10-03, review H1): REFUSED changed nothing,
// and the group is held and open as it was; UNFINISHED means this device HAS left -- the group is
// closed and no longer held -- and only the erase on the disk is still owed, which calling this
// again or the next urnet_message_device_restore finishes. A caller that treated UNFINISHED as a
// refusal would go on holding a closed group, and every send into it would fail.
//
// AN OWNER HOLDING ITS IDENTITY'S LAST LEAF, WITH SOMEBODY ELSE IN THE GROUP, IS REFUSED (MASTER
// section 11): transfer ownership first with urnet_message_group_transfer_ownership. A caller that
// wants to OFFER the transfer asks the roster before it asks this.
//
// THE RESULT IS NAMED AND PRESET TO FAILED (re-check R3): cgoGuard recovers a panic, and an unnamed
// result then answers its zero value, which is URNET_MESSAGE_FORGET_OK -- and a caller told OK
// releases its handles and treats the device as having left.
//
//export urnet_message_device_forget_group
func urnet_message_device_forget_group(self C.uint64_t, groupId *C.uint8_t, groupIdLen C.int32_t, outError **C.char) (kind C.int32_t) {
	kind = C.int32_t(messageForgetFailed)
	defer cgoGuard("urnet_message_device_forget_group")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_forget_group")
	if !ok || self_ == nil {
		return C.int32_t(messageForgetFailed)
	}
	err := self_.ForgetGroup(goBytes(groupId, groupIdLen))
	setErrorOut(outError, err)
	return C.int32_t(messageForgetKindOf(err))
}

// THE FOUR ANSWERS OF urnet_message_device_forget_group, held equal to the header's
// URNET_MESSAGE_FORGET_* by name and value (TestTheHeaderDefinesExactlyTheForgetKinds).
const (
	messageForgetOk         int32 = 0
	messageForgetRefused    int32 = 1
	messageForgetUnfinished int32 = 2
	messageForgetFailed     int32 = 3
)

// messageForgetKindOf is the projection of ForgetGroup's error onto the kinds above. UNFINISHED and
// FAILED are named by their sentinels. Every other answer ForgetGroup gives -- the owner refusal, a
// roster it could not read, a leave mark it could not write, a store it could not ask -- is given
// before anything is changed, so it is REFUSED.
func messageForgetKindOf(err error) int32 {
	switch {
	case err == nil:
		return messageForgetOk
	case errors.Is(err, urmessage.ErrForgetUnfinished):
		return messageForgetUnfinished
	case errors.Is(err, urmessage.ErrGroupNotHeld):
		return messageForgetFailed
	}
	return messageForgetRefused
}

// urnet_message_device_join joins the group an invite carries. A group joined ABOVE EPOCH ONE will
// not seal until urnet_message_group_receive has run once over it -- that is urmessage's
// ErrStreamFloorUnheld, and it is what bounds a leaf a removed member may have stood at. It is the
// exact mirror of the restore contract two functions up, and it is written here because join->send
// is the flow a C caller writes by default.
//
// WHY IT EXISTS, IN ONE DERIVATION. A joiner lands on the leftmost BLANK leaf (RFC 9420 section
// 7.7), which may be a leaf a removed member stood at, and its sender_handle takes no epoch and no
// identity -- so it inherits that member's sixteen octets byte for byte, and the server already
// holds a stream claim at every index that member spent. A first send with no receive behind it
// collides with one of those claims, and the refusal that follows is STICKY for the life of the
// process: that group can never be sent to again from this device. One receive is what makes the two
// occupants' index ranges disjoint.
//
// WHAT A CALLER DOES ABOUT IT: call urnet_message_group_receive once, then send. A group joined at
// epoch one never carries the refusal -- and every group this ABI can create is one, because
// urnet_message_group_add_member adds exactly one member before urnet_message_group_open -- so a
// caller that receives once before its first send is correct in both cases and needs no epoch test.
// An invite minted by a client that is not this ABI can be above epoch one, which is why the rule is
// stated on the join and not on the add.
//
// THIS ABI HAS NO TYPED ERROR CHANNEL TO BRANCH ON, and that is measured rather than assumed: every
// call here reports through char** out_error, and the only typedef in urnetwork_message.h is the
// connect-attempt callback. What a caller gets is the sentence, carrying urmessage's own stable
// prefix. The remedy needs no code, because it is the same one receive either way.
//
//export urnet_message_device_join
func urnet_message_device_join(self C.uint64_t, ctx C.uint64_t, invite C.uint64_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_device_join")
	self_, ok := resolveHandle[*urmessage.Device](uint64(self), "urnet_message_device_join")
	if !ok || self_ == nil {
		return 0
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_device_join")
	if !ok {
		return 0
	}
	invite_, ok := resolveHandle[*urmessage.Invite](uint64(invite), "urnet_message_device_join")
	if !ok || invite_ == nil {
		return 0
	}
	group, err := self_.Join(ctx_, invite_)
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(group))
}

// ── the invite, which is secret in full ─────────────────────────────────────────────────────

//export urnet_message_group_add_member
func urnet_message_group_add_member(self C.uint64_t, keyPackage *C.uint8_t, keyPackageLen C.int32_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_group_add_member")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_add_member")
	if !ok || self_ == nil {
		return 0
	}
	invite, err := self_.AddMember(goBytes(keyPackage, keyPackageLen))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(invite))
}

// urnet_message_group_add_member_and_publish BRINGS SOMEBODY INTO A GROUP THAT IS ALREADY OPEN,
// which urnet_message_group_add_member above cannot do: that one builds the FOUNDING commit and
// is refused once the group is open. This one stages a commit at the current epoch, publishes it,
// and merges only on the server's REASON_OK -- so a refused or lost commit leaves this group
// exactly where it was.
//
// IT IS ROLE-GATED ON THE SENDING SIDE, which is MASTER section 11 (ledger item 242 R2, ruling 1):
// adding a new identity is an ADMIN's or the OWNER's, and an identity's own second device is its
// own to add at any role. A member's add is refused HERE, before anything is built, with the same
// sentence every receiver would have judged it by.
//
// THE FAILURE CHANNEL IS THE SENTENCE AND NOT A KIND, unlike urnet_message_group_set_role, and
// that is a deliberate difference rather than an oversight: this call's RESULT is the invite
// handle the caller needs in order to encode an invitation, so the return value is spent on that.
// What a caller loses is the cheap branch between a lost epoch race (worth retrying as-is) and a
// refusal by role (never worth retrying), and what it gets instead is urmessage's own stable
// prefix in out_error, which names both. A caller that must branch should match the prefix.
//
//export urnet_message_group_add_member_and_publish
func urnet_message_group_add_member_and_publish(self C.uint64_t, ctx C.uint64_t, keyPackage *C.uint8_t, keyPackageLen C.int32_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_group_add_member_and_publish")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_add_member_and_publish")
	if !ok || self_ == nil {
		return 0
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_add_member_and_publish")
	if !ok {
		return 0
	}
	invite, err := self_.AddMemberAndPublish(ctx_, goBytes(keyPackage, keyPackageLen))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(invite))
}

// urnet_message_invite_encode is the buffer-out pattern. WHAT COMES OUT IS KEY MATERIAL: two of
// an invite's four fields are secret (pq_secret, and the MLS init secret inside the Welcome), so
// an invite that reaches a third party is a group that third party is in. Move it the way you
// would move a private key, once, over a channel that is already authenticated and already
// confidential, and destroy it afterwards.
//
//export urnet_message_invite_encode
func urnet_message_invite_encode(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_message_invite_encode")
	self_, ok := resolveHandle[*urmessage.Invite](uint64(self), "urnet_message_invite_encode")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	encoded, err := self_.Encode()
	if err != nil {
		setErrorOut(outError, err)
		if inoutLen != nil {
			*inoutLen = 0
		}
		return C.bool(false)
	}
	return copyOut(out, inoutLen, encoded)
}

//export urnet_message_parse_invite
func urnet_message_parse_invite(encoded *C.uint8_t, encodedLen C.int32_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_parse_invite")
	invite, err := urmessage.ParseInvite(goBytes(encoded, encodedLen))
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(invite))
}

// ── the group ───────────────────────────────────────────────────────────────────────────────

//export urnet_message_group_open
func urnet_message_group_open(self C.uint64_t, ctx C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_message_group_open")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_open")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_open")
	if !ok {
		return C.bool(false)
	}
	if err := self_.Open(ctx_); err != nil {
		setErrorOut(outError, err)
		return C.bool(false)
	}
	return C.bool(true)
}

// urnet_message_group_send seals one body and BLOCKS on its submit. The body is counted octets
// and crosses byte for byte: it is not a char*, it is not read as UTF-8, and it may contain
// 0x00. See the body decision at the top of this file.
//
// The returned string is this message's metadata as json and DOES NOT CARRY THE BODY -- the
// caller already has the body, and a json-carried body would be silently mangled wherever it is
// not valid UTF-8. Free it with urnet_free_string.
//
//export urnet_message_group_send
func urnet_message_group_send(self C.uint64_t, ctx C.uint64_t, body *C.uint8_t, bodyLen C.int32_t, outError **C.char) *C.char {
	defer cgoGuard("urnet_message_group_send")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_send")
	if !ok || self_ == nil {
		return nil
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_send")
	if !ok {
		return nil
	}
	sent, err := self_.Send(ctx_, string(goBytes(body, bodyLen)))
	if err != nil {
		setErrorOut(outError, err)
		return nil
	}
	return cJson(messageInfoOf(messageEntryOf(sent)), "urnet_message_group_send")
}

// ── the four verbs that name another message ────────────────────────────────────────────────
//
// A REPLY, A REACTION, AN UN-REACTION AND A TOMBSTONE. Until these landed urmessage had SendReply,
// React, Unreact and Delete and NO export called them, so a C caller could RENDER a reply, a
// reaction and a deletion -- kind, reply_to_id, deleted and the reaction list all cross in
// messageInfo -- and could not MAKE one. That is the asymmetry the header used to name as the next
// thing this file owed, and these four are it.
//
// A TARGET CROSSES AS COUNTED OCTETS, WHICH IS THIS FILE'S RULE FOR EVERY BINARY VALUE GOING IN and
// is what group_id and key_package already do. A message_id is 32 octets of derivation output
// rather than text, and the length is the caller's to pass rather than this side's to assume:
// urmessage answers ErrContentMalformed by name for any other width, which is a sentence a caller
// can act on.
//
// WHERE A CALLER GETS ONE, stated because the two directions are NOT symmetric and a reader will go
// looking. A message_id comes BACK as 64 lower case hex characters, in the message_id field of
// urnet_message_list_info's json -- metadata crosses as json, and a buffer-out call for 32 octets a
// renderer reads once per row would be a second call per message. So a caller decodes that hex once
// into the 32 octets it passes here. urnetwork_message.h says so beside the declarations.
//
// THEY ALL BLOCK AND ALL TAKE A CANCEL HANDLE, exactly as urnet_message_group_send does and for the
// same reason: each one seals a record and waits on its submit.
//
// WHAT EACH ANSWERS IS THE RECORD'S OWN METADATA -- the same messageInfo json
// urnet_message_group_send answers -- or NULL with out_error set. FOR THE THREE THAT ARE NOT A
// REPLY, THAT VALUE IS NOT A LINE OF THE CONVERSATION: a reaction and a tombstone CHANGE another
// message and add no entry of their own, so what comes back is there to give a caller the record_id
// and the message_id of what it just sent -- the two things a later un-reaction and any log would
// need -- and NOT to be appended to a view. The change itself shows up on the TARGET, through
// urnet_message_group_messages.

// urnet_message_group_send_reply seals one line of text that NAMES the message it answers, and
// BLOCKS on its submit.
//
// THE QUOTED TEXT NEVER TRAVELS. A reply carries its parent's message_id and renders by looking the
// parent up, which is what keeps a reply from being a second copy of a line the group already paid
// for -- and the parent may legitimately be unavailable: deleted, pruned, or not yet fetched by the
// device showing the reply. So the parent is NOT required to be present here, which is the one
// place this differs from react and delete below: a reply is a message in its own right.
//
// The body is counted octets and crosses byte for byte, exactly as urnet_message_group_send's does.
// A reply's ceiling is lower than a plain message's by the 32 octets of the name, which come out of
// the same plaintext budget as the text.
//
//export urnet_message_group_send_reply
func urnet_message_group_send_reply(self C.uint64_t, ctx C.uint64_t, replyTo *C.uint8_t, replyToLen C.int32_t, body *C.uint8_t, bodyLen C.int32_t, outError **C.char) *C.char {
	defer cgoGuard("urnet_message_group_send_reply")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_send_reply")
	if !ok || self_ == nil {
		return nil
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_send_reply")
	if !ok {
		return nil
	}
	sent, err := self_.SendReply(ctx_, goBytes(replyTo, replyToLen), string(goBytes(body, bodyLen)))
	if err != nil {
		setErrorOut(outError, err)
		return nil
	}
	return cJson(messageInfoOf(messageEntryOf(sent)), "urnet_message_group_send_reply")
}

// urnet_message_group_react seals one REACTION_ADD naming a message this group holds, and BLOCKS on
// its submit.
//
// THE EMOJI IS A char* AND THAT IS NOT THE BODY RULE BEING BROKEN. The body rule exists because a
// body is octets from another device that may carry 0x00 and may not be UTF-8. An emoji is neither:
// urmessage validates it as valid UTF-8 of 1..MaxEmojiOctets octets BEFORE anything is sealed, and
// a NUL inside one makes it invalid UTF-8 -- so a char* cannot truncate one without the refusal
// firing first, which is the failure the body rule exists to prevent. What is NOT validated is
// "exactly one extended grapheme cluster from the pinned Unicode version", which needs a UAX-29
// dependency nobody has decided to take: a caller that passes two characters sends two characters
// to every member.
//
// A TARGET THIS DEVICE DOES NOT HOLD IS A REFUSAL AND NO RECORD IS SEALED. It is not a courtesy
// check: a reaction standing on an id nothing carries is a record every member holds for ever,
// waiting for a target that will not arrive. The same refusal covers a target that is a GAP -- a
// record this build could not show -- and one that is a reaction, a tombstone or a cover rather
// than a stored content message.
//
//export urnet_message_group_react
func urnet_message_group_react(self C.uint64_t, ctx C.uint64_t, target *C.uint8_t, targetLen C.int32_t, emoji *C.char, outError **C.char) *C.char {
	defer cgoGuard("urnet_message_group_react")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_react")
	if !ok || self_ == nil {
		return nil
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_react")
	if !ok {
		return nil
	}
	sent, err := self_.React(ctx_, goBytes(target, targetLen), goString(emoji))
	if err != nil {
		setErrorOut(outError, err)
		return nil
	}
	return cJson(messageInfoOf(messageEntryOf(sent)), "urnet_message_group_react")
}

// urnet_message_group_unreact seals one REACTION_REMOVE, and BLOCKS on its submit.
//
// IT CANCELS AN ADD WITH THE SAME (reactor, target, emoji) AND NOBODY ELSE'S. The reactor is the
// sender_handle, so until an identity system exists A SECOND DEVICE OF ONE PERSON CANNOT TAKE BACK
// THE FIRST'S REACTION: it seals under its own handle and the removal finds nothing of its own to
// cancel. That is a property of the alpha and not of this binding.
//
// IT IS A RECORD AND NOT AN UNDO. The ADD stays on the server; what this seals is a SECOND record
// saying the reaction no longer stands, and every member replays both in server order. So an
// un-reaction of a reaction that has not been fetched yet still lands correctly on every device.
//
//export urnet_message_group_unreact
func urnet_message_group_unreact(self C.uint64_t, ctx C.uint64_t, target *C.uint8_t, targetLen C.int32_t, emoji *C.char, outError **C.char) *C.char {
	defer cgoGuard("urnet_message_group_unreact")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_unreact")
	if !ok || self_ == nil {
		return nil
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_unreact")
	if !ok {
		return nil
	}
	sent, err := self_.Unreact(ctx_, goBytes(target, targetLen), goString(emoji))
	if err != nil {
		setErrorOut(outError, err)
		return nil
	}
	return cJson(messageInfoOf(messageEntryOf(sent)), "urnet_message_group_unreact")
}

// urnet_message_group_delete seals one TOMBSTONE naming a message of THIS DEVICE'S OWN, and BLOCKS
// on its submit.
//
// ONLY THIS DEVICE'S OWN, AND IT IS ENFORCED ON BOTH SIDES. A tombstone applies only if its
// sender_handle equals its target's: nothing in a record proves its sender wrote the message it
// NAMES, so a tombstone over somebody else's message is one every honest receiver ignores, and the
// honest thing is not to seal one. A call naming another member's message is refused here and emits
// no record.
//
// WHAT IT DOES NOT DO, because a caller will assume otherwise. It does not erase the record on the
// server -- there is no client-initiated server-side erase in v1. It does not clear the text: the
// target keeps its body and its body_len and `deleted` goes true beside them, because urmessage
// refuses to be the layer that throws away a user's data on a peer's say-so and the record is on
// the server either way. What a UI shows for a deleted line is the UI's decision and this abi does
// not make it.
//
//export urnet_message_group_delete
func urnet_message_group_delete(self C.uint64_t, ctx C.uint64_t, target *C.uint8_t, targetLen C.int32_t, outError **C.char) *C.char {
	defer cgoGuard("urnet_message_group_delete")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_delete")
	if !ok || self_ == nil {
		return nil
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_delete")
	if !ok {
		return nil
	}
	sent, err := self_.Delete(ctx_, goBytes(target, targetLen))
	if err != nil {
		setErrorOut(outError, err)
		return nil
	}
	return cJson(messageInfoOf(messageEntryOf(sent)), "urnet_message_group_delete")
}

// urnet_message_group_receive fetches §4.3.4's pages and BLOCKS while it does.
//
// A NON-ZERO RESULT AND A NON-NULL out_error CAN BOTH COME BACK, and a caller that reads an
// error as "nothing arrived" will drop real messages. urmessage returns its partial answers WITH
// the reason -- a page bound reached with more to come (ErrFetchIncomplete), a server that named
// a high water above everything it handed over (ErrFetchOmitted), a record given up on after
// every retry (ErrRecordAbandoned) -- and this export carries both halves rather than collapsing
// one into the other.
//
// Zero with no error is the ordinary polling answer: nothing new. Zero with an error is a fetch
// that returned nothing at all.
//
// AND ZERO WITH NO ERROR IS ALSO WHAT AN UNKNOWN self OR ctx HANDLE ANSWERS, which makes a dead
// handle look like a quiet conversation. That is this abi's convention everywhere -- a handle
// that does not resolve is a programming error, it is logged by name through glog and it is not
// an out_error -- and it is stated here rather than only inherited, because receive is the one
// export where the ambiguous answer is also the common one.
//
//export urnet_message_group_receive
func urnet_message_group_receive(self C.uint64_t, ctx C.uint64_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_group_receive")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_receive")
	if !ok || self_ == nil {
		return 0
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_receive")
	if !ok {
		return 0
	}
	messages, err := self_.Receive(ctx_)
	if err != nil {
		setErrorOut(outError, err)
	}
	return C.uint64_t(newMessageList(messages))
}

// urnet_message_group_messages is every message this group has sent or received, in the order it
// learned them.
//
//export urnet_message_group_messages
func urnet_message_group_messages(self C.uint64_t) C.uint64_t {
	defer cgoGuard("urnet_message_group_messages")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_messages")
	if !ok || self_ == nil {
		return 0
	}
	return C.uint64_t(newMessageList(self_.Messages()))
}

//export urnet_message_group_id
func urnet_message_group_id(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_message_group_id")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_id")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	return copyOut(out, inoutLen, self_.Id())
}

//export urnet_message_group_epoch
func urnet_message_group_epoch(self C.uint64_t) C.uint64_t {
	defer cgoGuard("urnet_message_group_epoch")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_epoch")
	if !ok || self_ == nil {
		return 0
	}
	return C.uint64_t(self_.Epoch())
}

//export urnet_message_group_is_open
func urnet_message_group_is_open(self C.uint64_t) C.bool {
	defer cgoGuard("urnet_message_group_is_open")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_is_open")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	return C.bool(self_.IsOpen())
}

// urnet_message_group_stats is what this group has SEEN, as json. Free with urnet_free_string.
// It exists so that "nothing arrived" and "something arrived and this build would not open it"
// are two readings rather than one silence; every counter urmessage keeps is carried.
//
//export urnet_message_group_stats
func urnet_message_group_stats(self C.uint64_t) *C.char {
	defer cgoGuard("urnet_message_group_stats")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_stats")
	if !ok || self_ == nil {
		return nil
	}
	stats := self_.Stats()
	return cJson(&messageGroupStats{
		Fetched:         stats.Fetched,
		Opened:          stats.Opened,
		SkippedCeremony: stats.SkippedCeremony,
		SkippedOwn:      stats.SkippedOwn,
		OpenedOwn:       stats.OpenedOwn,
		OwnWithoutCopy:  stats.OwnWithoutCopy,
		SkippedSeen:     stats.SkippedSeen,
		Unopened:        stats.Unopened,
		Omitted:         stats.Omitted,
		SkippedClass:    stats.SkippedClass,
		WrapOpened:      stats.WrapOpened,
		WrapMissing:     stats.WrapMissing,
		WrapUnreadable:  stats.WrapUnreadable,
		WrapOrphaned:    stats.WrapOrphaned,
		GapMalformed:    stats.GapMalformed,
		GapUnsupported:  stats.GapUnsupported,
		GapOutOfWindow:  stats.GapOutOfWindow,
		OpenedPastEpoch: stats.OpenedPastEpoch,

		HiddenObserver:          stats.HiddenObserver,
		ObserverReactionRefused: stats.ObserverReactionRefused,
		RoleUndeterminable:      stats.RoleUndeterminable,

		Ingested:          stats.Ingested,
		CommitRefused:     stats.CommitRefused,
		CommitRefusedOwn:  stats.CommitRefusedOwn,
		FailedOpen:        stats.FailedOpen,
		Submitted:         stats.Submitted,
		Rebound:           stats.Rebound,
		Pages:             stats.Pages,
		Unattested:        stats.Unattested,
		StreamFloorSeeded: stats.StreamFloorSeeded,

		UnopenedUnattributed: stats.UnopenedUnattributed,
	}, "urnet_message_group_stats")
}

// ── RULING 52's STATE, PROJECTED SO AN APP CAN RENDER IT ────────────────────────────────────
//
// WHY THIS IS A GETTER AND NOT A FIELD OF AN ERROR. urmessage answers urmessage.ErrRemovedFromGroup
// from Receive, from Send and from every commit verb, and a C caller cannot branch on a sentinel: what
// crosses this boundary is an out_error SENTENCE, plus a commit KIND on the three verbs that have one.
// A receive that failed because this device is not in the group any more is indistinguishable at the
// abi from one that failed because the network went away -- and the two are opposite instructions to a
// UI, since one clears on a retry and the other never will. So the state is readable directly, at any
// time, with no call having had to fail first.
//
// WHY IT IS JSON RATHER THAN A bool BESIDE A uint64_t. The two values are ONE fact read under one lock
// (see urmessage.Group.Removal), and two getters would let a caller render a pair this group never held
// at once. The keys are documented in the header and a go test in this directory holds that list
// against the json's own, exactly as it does for the stats.
//
// WHAT IT IS NOT: the other two permanent states. A group that HALTED on an invalid commit (ruling 41)
// and one that went DARK on a wrap that never arrived (ruling 38) are still only sentences in
// out_error at this boundary. They are different screens from this one and they are owed their own
// projection; this export is ruling 52's and says so rather than pretending to be a general one.
//
// AND THE ONE PLACE THAT COSTS SOMETHING IS NAMED RATHER THAN LEFT TO BE DISCOVERED, because ledger
// item 259's adversary pass discovered it. The device a DIGEST-LESS removal NAMES reads `removed: false`
// here. That is the right answer and not a hole in this projection: urmessage refuses such a commit
// before applying it, for the victim exactly as for every survivor, so the victim was not removed -- it
// REFUSED, its leaf is still in the tree it is standing in, and it is still a member at the epoch it
// refused from. Saying "you are no longer in this group" would report a membership change the device
// did not accept and its own MLS state does not carry.
//
// WHAT IT DOES COST is that such a device is in the HALT, and the halt has no projection here at all --
// so a UI reading only this export can render NEITHER screen for it and has nothing but an out_error
// sentence to go on. That is ruling 41's track and not ruling 52's: closing it is a header key, a ctest
// reader and a ruling on whether an unauthorized commit and an unrotated removal are one state or two.
// It is written here because this is where a reader meets the false-looking `false`.
// urmessage's TestTheVictimOfADigestLessRemovalIsHaltedAndReadsNoRemoval drives the reading this
// export makes, with a properly rotated removal of the same victim beside it as the control.
//
//export urnet_message_group_removal
func urnet_message_group_removal(self C.uint64_t) *C.char {
	defer cgoGuard("urnet_message_group_removal")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_removal")
	if !ok || self_ == nil {
		return nil
	}
	epoch, removal := self_.Removal()
	return cJson(&messageGroupRemoval{
		Removed:      removal != nil,
		RemovedEpoch: epoch,
	}, "urnet_message_group_removal")
}

//export urnet_message_group_close
func urnet_message_group_close(self C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_message_group_close")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_close")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	if err := self_.Close(); err != nil {
		setErrorOut(outError, err)
		return C.bool(false)
	}
	return C.bool(true)
}

// ── the roster and the two role verbs (MASTER §11, ledger item 242's R3) ────────────────────
//
// THE ROLE MODEL IS LIVE UNDERNEATH THIS and until these exports a C caller could see none of it:
// urmessage judges every commit it ingests and every commit it is asked to build against §11's
// rules, keeps a roster with a role per member, and has two policy verbs -- and the Windows roster
// (Spec C screens 15 and 16) had nothing to read and nothing to call. What crosses here is the
// read surface and the two verbs, in this abi's own shapes: the roster is a list handle with
// count and info(index) json, exactly as a page of messages is, and the role is a string.
//
// ── DECISION: A COMMIT VERB ANSWERS A KIND, NOT A bool ──────────────────────────────────────
//
// Every other verb in this file answers true/false or a handle/NULL, with out_error carrying a
// sentence. A policy commit has THREE failures a caller has to act on differently, and a sentence
// cannot be branched on:
//
//   REFUSED   this device's role does not permit the change. Nothing was built, nothing moved for
//             anybody, commit_refused_own moved by one. A UI says "you cannot do this" and does not
//             retry -- retrying answers the same. It is urmessage's ErrCommitUnauthorized, which
//             wraps the §11 rule that refused it, and the rule's own sentence is in out_error.
//   LOST      another member's commit closed this epoch first (MASTER §9.3's race). The staged
//             epoch was erased and the group is exactly where it was; the caller owes a
//             urnet_message_group_receive to follow the winner, then the same verb again. It is
//             urmessage's ErrCommitLost. A UI that showed this as an error would be wrong twice: the
//             change may still be right, and the way to make it is to try again.
//   INVALID   the request was malformed and refused by name before any rule was reached: a role
//             that is not admin/member/observer (ownership moves through transfer_ownership),
//             a transfer to the identity that already owns the group, an identity that is not
//             hex. Nothing counted. It is a caller bug and the sentence says which.
//   FAILED    everything else: the transport, a group that is not open or not yet reconciled,
//             a closed handle. out_error says what.
//
// So the two verbs answer an int32_t of URNET_MESSAGE_COMMIT_OK / _REFUSED / _LOST / _INVALID /
// _FAILED, with out_error set on everything but OK. An unknown self or ctx handle answers FAILED
// with out_error left NULL, which is this abi's convention for a handle that does not resolve
// (logged by name, not an out_error). The mapping is messageCommitKindOf, and a test holds the
// header's defines equal to the go constants by name and value.
//
// A SAME-ROLE set_role IS OK AND MOVES NOTHING (ruling 15): urmessage answers nil without building
// a commit, so the epoch does not change and there is nothing to fetch.

// The kinds. THE HEADER'S URNET_MESSAGE_COMMIT_* DEFINES ARE THESE, by name and by value, and
// TestTheHeaderDefinesExactlyTheCommitKinds holds them equal.
const (
	messageCommitOk      int32 = 0
	messageCommitRefused int32 = 1
	messageCommitLost    int32 = 2
	messageCommitInvalid int32 = 3
	messageCommitFailed  int32 = 4
)

// messageCommitKindOf is the projection of a commit verb's error onto the kinds above. The order
// matters only where two sentinels could both match, and none do: ErrCommitLost wraps
// ErrSubmitRefused and not ErrCommitUnauthorized; the INVALID sentinels wrap nothing of the others.
//
// THE INVALID SET IS EVERY REQUEST A VERB REFUSES BY NAME BEFORE A RULE IS REACHED, and it grew by
// three with urnet_message_group_remove_member (ledger item 258): urmessage.ErrRemoveSelf,
// urmessage.ErrRemoveOwner and urmessage.ErrNoSuchMember. Each is a caller bug rather than a role
// answer -- nothing is built, nothing is counted, and retrying answers the same -- which is exactly
// what the header's INVALID paragraph promises, and what keeps them off REFUSED, whose contract says
// "this device's role does not permit the change" and whose counter moved.
func messageCommitKindOf(err error) int32 {
	switch {
	case err == nil:
		return messageCommitOk
	case errors.Is(err, urmessage.ErrCommitUnauthorized):
		return messageCommitRefused
	case errors.Is(err, urmessage.ErrCommitLost):
		return messageCommitLost
	case errors.Is(err, urmessage.ErrRoleNotSettable), errors.Is(err, urmessage.ErrAlreadyOwner),
		errors.Is(err, urmessage.ErrRemoveSelf), errors.Is(err, urmessage.ErrRemoveOwner),
		errors.Is(err, urmessage.ErrNoSuchMember), errors.Is(err, errIdentityNotHex):
		return messageCommitInvalid
	}
	return messageCommitFailed
}

// errIdentityNotHex is the INVALID refusal for an identity_pub_hex that does not decode. It is
// this file's own rather than urmessage's because the hex is this abi's encoding: urmessage takes
// octets.
var errIdentityNotHex = errors.New("urnet_message: identity_pub_hex is not the lower case hex of a member's identity_pub as urnet_message_member_list_info carries it")

// messageIdentityOf decodes the identity a verb names. An empty string is refused too: an empty
// identity holds no leaf and would be answered as a phantom by the rules, which is a sentence
// about roles for what is a missing argument.
func messageIdentityOf(identityPubHex *C.char) ([]byte, error) {
	text := goString(identityPubHex)
	if text == "" {
		return nil, fmt.Errorf("%w: it is empty", errIdentityNotHex)
	}
	identity, err := hex.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errIdentityNotHex, err)
	}
	return identity, nil
}

// memberInfo is one member of the roster as json. THE FIELD SET IS urmessage.Member's, held by
// TestTheMemberInfoCarriesEveryFieldUrmessageKeeps the way messageInfo is held to
// urmessage.Message: a field added there and not here reaches a C caller as nothing at all.
type memberInfo struct {
	// The member's leaf in the ratchet tree, which is what a role is read at and what
	// "sender leaf index" in a message info refers to.
	LeafIndex uint32 `json:"leaf_index"`
	// The 16 octets this member's records carry, lower case hex: the same value as a message
	// info's sender_handle, so a caller joins a line to a roster row on this field.
	SenderHandle string `json:"sender_handle"`
	// The credential identity the leaf carries -- the Ed25519 identity public key -- as lower
	// case hex. IT IS THE VALUE THE TWO VERBS TAKE: pass it back as identity_pub_hex. One
	// identity may hold several leaves (its devices) and then appears once per leaf, with the
	// same role on each.
	IdentityPub string `json:"identity_pub"`
	// "owner", "admin", "member" or "observer": the role the live policy gives the identity. An
	// identity the policy does not name is "member" (MASTER §11, ruling 8).
	Role string `json:"role"`
	// True on this device's own leaf.
	Mine bool `json:"mine"`
}

func memberInfoOf(member urmessage.Member) *memberInfo {
	return &memberInfo{
		LeafIndex:    member.LeafIndex,
		SenderHandle: hex.EncodeToString(member.SenderHandle),
		IdentityPub:  hex.EncodeToString(member.IdentityPub),
		Role:         member.Role,
		Mine:         member.Mine,
	}
}

// memberList is the roster at one instant, in leaf order. urmessage.Members already answers a
// copy, so nothing here can move under a caller.
type memberList struct {
	members []urmessage.Member
}

// urnet_message_group_members is the roster: every member of this group at its current epoch, in
// leaf order, each with the role the live policy gives it, as a member list handle. It is what a
// UI shows, and it is exactly what this device would be judged by were it to commit now. 0 with
// out_error set when the roster cannot be read (a closed group); 0 with no error for an unknown
// handle. A group always has at least its own leaf, so a non-zero handle is never empty.
//
//export urnet_message_group_members
func urnet_message_group_members(self C.uint64_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_group_members")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_members")
	if !ok || self_ == nil {
		return 0
	}
	members, err := self_.Members()
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	if len(members) == 0 {
		return 0
	}
	return C.uint64_t(newHandle(&memberList{members: members}))
}

// urnet_message_group_my_role is the role the live policy gives this device's own identity:
// "owner", "admin", "member" or "observer". It is what this device may commit, read at its own
// leaf, which is the reading every receiver takes of a commit this device makes. NULL with
// out_error set when it cannot be read. Free with urnet_free_string.
//
//export urnet_message_group_my_role
func urnet_message_group_my_role(self C.uint64_t, outError **C.char) *C.char {
	defer cgoGuard("urnet_message_group_my_role")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_my_role")
	if !ok || self_ == nil {
		return nil
	}
	role, err := self_.MyRole()
	if err != nil {
		setErrorOut(outError, err)
		return nil
	}
	return cString(role)
}

// urnet_message_group_set_role makes one identity an "admin", a "member" or an "observer" in one
// commit, and BLOCKS on its submit. It answers a URNET_MESSAGE_COMMIT_* kind; see the decision
// above. identity_pub_hex is the identity_pub a member info carries. "owner" is INVALID here:
// ownership moves through urnet_message_group_transfer_ownership. Who may set what is §11's
// table -- the owner the admin set, an admin member/observer -- and a caller that may not is
// REFUSED with the rule's sentence; a caller that is neither admin nor owner is REFUSED whatever
// it asked for (ruling 15). Naming the role the identity already holds is OK and moves nothing.
//
//export urnet_message_group_set_role
func urnet_message_group_set_role(self C.uint64_t, ctx C.uint64_t, identityPubHex *C.char, role *C.char, outError **C.char) C.int32_t {
	defer cgoGuard("urnet_message_group_set_role")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_set_role")
	if !ok || self_ == nil {
		return C.int32_t(messageCommitFailed)
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_set_role")
	if !ok {
		return C.int32_t(messageCommitFailed)
	}
	identity, err := messageIdentityOf(identityPubHex)
	if err == nil {
		err = self_.SetRole(ctx_, identity, goString(role))
	}
	setErrorOut(outError, err)
	return C.int32_t(messageCommitKindOf(err))
}

// urnet_message_group_transfer_ownership makes one identity the OWNER and this device's identity
// -- the outgoing owner -- an ADMIN, in one commit (§11: "the outgoing owner becomes an ADMIN"),
// and BLOCKS on its submit. It answers a URNET_MESSAGE_COMMIT_* kind. The new owner must already
// hold a leaf: a stranger is REFUSED as a phantom (R0c), and naming the identity that already
// owns the group is INVALID. Anybody but the owner is REFUSED (R5).
//
//export urnet_message_group_transfer_ownership
func urnet_message_group_transfer_ownership(self C.uint64_t, ctx C.uint64_t, identityPubHex *C.char, outError **C.char) C.int32_t {
	defer cgoGuard("urnet_message_group_transfer_ownership")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_transfer_ownership")
	if !ok || self_ == nil {
		return C.int32_t(messageCommitFailed)
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_transfer_ownership")
	if !ok {
		return C.int32_t(messageCommitFailed)
	}
	identity, err := messageIdentityOf(identityPubHex)
	if err == nil {
		err = self_.TransferOwnership(ctx_, identity)
	}
	setErrorOut(outError, err)
	return C.int32_t(messageCommitKindOf(err))
}

// urnet_message_group_remove_member takes one identity out of the group -- EVERY device leaf it
// holds and its entry in the group's policy, in ONE commit -- and BLOCKS on the submit. It answers a
// URNET_MESSAGE_COMMIT_* kind; see the decision above. identity_pub_hex is the identity_pub a member
// info carries, so a roster row is the whole of what a caller needs.
//
// Who may remove whom is MASTER section 11's table and is decided by the SAME predicate every
// receiver runs, before anything is built: a member or an observer is REFUSED, and only the owner may
// remove an admin. Three requests are INVALID rather than REFUSED, each refused by name: the
// identity that OWNS the group (ownership moves through urnet_message_group_transfer_ownership
// first, and the outgoing owner is then an admin the new owner may remove), THIS device's own
// identity (leaving is a product flow and not this call -- ledger item 257's ruling 48 -- and the
// text says so), and an identity no leaf carries.
//
//export urnet_message_group_remove_member
func urnet_message_group_remove_member(self C.uint64_t, ctx C.uint64_t, identityPubHex *C.char, outError **C.char) C.int32_t {
	defer cgoGuard("urnet_message_group_remove_member")
	self_, ok := resolveHandle[*urmessage.Group](uint64(self), "urnet_message_group_remove_member")
	if !ok || self_ == nil {
		return C.int32_t(messageCommitFailed)
	}
	ctx_, ok := messageCtx(ctx, "urnet_message_group_remove_member")
	if !ok {
		return C.int32_t(messageCommitFailed)
	}
	identity, err := messageIdentityOf(identityPubHex)
	if err == nil {
		err = self_.RemoveMember(ctx_, identity)
	}
	setErrorOut(outError, err)
	return C.int32_t(messageCommitKindOf(err))
}

// ── the list handles ────────────────────────────────────────────────────────────────────────
//
// A []*Group, a []*Message and a []Member cross as ONE handle with indexed accessors rather than
// as N handles or as one json blob. N handles would make a 600 message page 600 things for a caller
// to release; one json blob cannot carry a body (see the body decision). An EMPTY slice is
// handle 0, so the ordinary "nothing new" poll costs the caller no release at all, and every
// accessor answers 0/NULL/false on handle 0 rather than failing.

type groupList struct {
	groups []*urmessage.Group
}

func newGroupList(groups []*urmessage.Group) uint64 {
	if len(groups) == 0 {
		return 0
	}
	return newHandle(&groupList{groups: groups})
}

//export urnet_message_group_list_count
func urnet_message_group_list_count(self C.uint64_t) C.int32_t {
	defer cgoGuard("urnet_message_group_list_count")
	self_, ok := resolveHandle[*groupList](uint64(self), "urnet_message_group_list_count")
	if !ok || self_ == nil {
		return 0
	}
	return C.int32_t(len(self_.groups))
}

// urnet_message_group_list_at hands out a NEW handle onto the group at index, which the caller
// releases. Two calls at one index are two handles onto one group; releasing either leaves the
// group alive under the other.
//
//export urnet_message_group_list_at
func urnet_message_group_list_at(self C.uint64_t, index C.int32_t) C.uint64_t {
	defer cgoGuard("urnet_message_group_list_at")
	self_, ok := resolveHandle[*groupList](uint64(self), "urnet_message_group_list_at")
	if !ok || self_ == nil {
		return 0
	}
	if index < 0 || int(index) >= len(self_.groups) {
		return 0
	}
	return C.uint64_t(newHandle(self_.groups[index]))
}

// messageEntry is one message in a list handle, WITH THE TWO FIELDS urmessage REWRITES IN PLACE
// COPIED AT THE INSTANT THE LIST WAS BUILT.
//
// WHY A SNAPSHOT AND NOT THE LIVE MESSAGE. urmessage.Group.Messages says "the messages themselves
// are shared and are not written after they are appended", and since the content envelope that
// sentence is false for exactly two fields: Group.reapplyLocked rewrites Deleted and Reactions on a
// message that is ALREADY in the log, every time a reaction or a tombstone for it arrives. This abi
// is polled from one thread while it is rendered from another -- that is the whole threading
// decision at the top of this file -- so a list handle that read those two fields live would answer
// urnet_message_list_reaction_count and urnet_message_list_reaction_info from two different
// instants: a C caller that looped `for k in 0..count` would read past the end of a list that had
// just shrunk, or miss the entry that had just been added, on a conversation nobody was doing
// anything unusual to.
//
// SO A LIST HANDLE IS ONE INSTANT OF THE CONVERSATION and every accessor on it agrees. Re-reading
// is one more urnet_message_group_messages call, which is what a render loop does anyway.
//
// WHAT IT DOES NOT FIX, STATED RATHER THAN IMPLIED AND MEASURED RATHER THAN SUSPECTED. The copy
// below reads those two fields WITHOUT urmessage's group lock, because urmessage offers no way to
// take one: Group.Messages copies the SLICE under the lock and hands back live messages. A probe
// that rendered Group.Messages on one goroutine while another called Group.Receive reported two
// data races over one short conversation under -race -- both of them the writes in reapplyLocked,
// at urmessage/group.go:2511 (`held.Deleted = false`) and :2512 (`held.Reactions = nil`), reached
// through Receive -> commitWalkLocked -> rebuildDirtyLocked.
//
// THAT RACE IS urmessage's AND IT IS OLDER THAN THIS FILE: any Go caller that renders while it
// polls has it, and closing it means changing what Group.Messages hands out, which belongs to the
// package that owns the lock rather than to a hand projection outside it. What this snapshot
// removes is the half that IS this file's: two accessors on one list handle answering a C caller
// from two different instants.
type messageEntry struct {
	message   *urmessage.Message
	deleted   bool
	reactions []urmessage.Reaction
}

// messageEntryOf takes that instant. A nil message is a nil entry and every reader below answers
// nothing for it.
func messageEntryOf(message *urmessage.Message) messageEntry {
	if message == nil {
		return messageEntry{}
	}
	return messageEntry{
		message: message,
		deleted: message.Deleted,
		// append onto nil rather than assigning the slice: assigning would share the array
		// with a message a later reapplyLocked appends into.
		reactions: append([]urmessage.Reaction(nil), message.Reactions...),
	}
}

type messageList struct {
	entries []messageEntry
}

func newMessageList(messages []*urmessage.Message) uint64 {
	if len(messages) == 0 {
		return 0
	}
	entries := make([]messageEntry, 0, len(messages))
	for _, message := range messages {
		entries = append(entries, messageEntryOf(message))
	}
	return newHandle(&messageList{entries: entries})
}

// messageInfo is one message WITHOUT its body and WITHOUT its reactions. The field names are
// snake_case to match the json every other data type in this abi crosses as; urmessage's own
// structs carry no json tags, so this is a projection rather than a marshal of the type -- and the
// projection is also what keeps the body out of json, which is the point.
//
// IT IS A HAND PROJECTION AND THAT IS WHY NO FIELD ARRIVES BY DEFAULT, which is the cause ledger
// item 236 names: Kind, ReplyToId, Deleted, Reactions and Gap all landed in urmessage.Message and
// reached a C caller as nothing at all, for months, with every test green.
// TestTheMessageInfoCarriesEveryFieldUrmessageKeeps is what makes the next one loud instead.
type messageInfo struct {
	RecordId uint64 `json:"record_id"`
	// 3.1's sender_handle, 16 octets, lower case hex. It is the routing identity of the member
	// that sealed the record and IT IS NOT A NAME: the alpha has no identity system.
	SenderHandle string `json:"sender_handle"`
	// THE CREDENTIAL IDENTITY OF THE MEMBER THAT SIGNED THIS RECORD, at the epoch it was SEALED
	// at, lower case hex, and "" on a record that did not open. It is the same value
	// urnet_message_group_members answers as identity_pub, so a caller joins a LINE to a ROSTER
	// ROW on this and never on sender_handle.
	//
	// THAT IS NOT A PREFERENCE, IT IS ledger item 245. sender_handle is derived from the LEAF
	// alone and the group's handle key never rotates, so a member added onto a removed member's
	// leaf carries the removed member's sender_handle byte for byte -- two people, one label,
	// for ever. A caller keyed on sender_handle merges their two histories into one row and
	// attributes each to the other. sender_identity is what MLS signs and is the only value here
	// that separates them.
	SenderIdentity string `json:"sender_identity"`
	// WHETHER THIS DEVICE SEALED THE RECORD, AND IT IS DECIDED ON sender_identity OR ON OCTETS
	// THIS DEVICE PRODUCED -- NEVER ON sender_handle, for the reason one field up. A record that
	// opened is `mine` when its sender_identity is this device's; a record that did NOT open is
	// `mine` only when this device sealed at that stream index and the record carries the
	// body_hash it sealed there, and such a record carries this device's sender_identity too. So
	// a caller may key a row on sender_identity and read `mine` beside it: the two agree on every
	// line, and neither of them is the sixteen octets two occupants of one leaf share.
	Mine bool `json:"mine"`
	// THE ROLE THE SENDER HELD AT THE EPOCH THIS RECORD WAS SEALED AT: "owner", "admin", "member"
	// or "observer", and "" on a record that did not open. Spec C §5.6's SenderRoleAtSend.
	//
	// IT IS A FACT ABOUT AN EPOCH AND NOT ABOUT NOW, which is why it is carried on the message
	// rather than looked up in the roster: urnet_message_group_members answers the roles the group
	// has TODAY, and a line written before a demotion was written under the role its sender held
	// THEN. A caller that joined a row to the roster on sender_handle and read the role off that
	// row would relabel history at every role change.
	//
	// "observer" IS THE ONE VALUE THAT ASKS A CALLER FOR ANYTHING: collapse the row to §5.1's
	// system line -- "A message from an observer was hidden." -- with the content one expansion
	// away. THE RECORD IS HERE AND ITS BODY IS INTACT: body_len is the real length and
	// urnet_message_list_body still hands it back, because a row dropped here is indistinguishable
	// from a record that never arrived. It is NOT a gap, and gap stays "".
	SenderRoleAtSend string `json:"sender_role_at_send"`
	SentAtMs         int64  `json:"sent_at_ms"`
	// The body's length in octets, which is what urnet_message_list_body will ask for.
	BodyLen int32 `json:"body_len"`
	// MASTER section 8.4.5's message_id, 32 octets as 64 lower case hex characters.
	//
	// IT IS THE NAME A LATER KIND QUOTES. A reply, a reaction, a tombstone or a read cursor has
	// to say WHICH message it is about, and record_id cannot be that name: record_id is the
	// SERVER's per-group counter, so a sender does not have it until the submit is answered and
	// a message whose submit response was lost carries zero. message_id is a function of the
	// record alone and every member derives the same value from the header it already holds.
	//
	// IT IS A NAME AND NOT AN AUTHENTICATION. The key it is derived under is group-shared, so
	// any member can compute any member's id at any position, including positions nobody has
	// written yet. What makes an id trustworthy is that the record it names OPENED.
	MessageId string `json:"message_id"`

	// ── what the content envelope said, which is what a conversation renders FROM ────────

	// The content kind at octet 0 of the application plaintext: what grammar the body and the
	// three fields below were read under. 1 is TEXT and 2 is REPLY; the header names the codes
	// this build knows, as URNET_MESSAGE_KIND_*.
	//
	// IT IS A NUMBER AND NOT A NAME, because a code this build does NOT know still has to cross:
	// urmessage renders an unassigned code as "0x0b" and a switch in C cannot be written against
	// that. A caller shows a placeholder for any code it does not handle, which is the whole of
	// the unknown-kind rule's rendering obligation.
	//
	// ON A GAP IT IS THE CODE THE RECORD ARRIVED UNDER AND NOT WHAT THE RECORD IS. A malformed
	// REPLY carries 2 here and is still a gap. Branch on gap, below, and not on this.
	Kind uint8 `json:"kind"`

	// WHY THIS POSITION IN THE CONVERSATION HOLDS A GAP RATHER THAN A MESSAGE, and "" on every
	// message that is a message. Spec A §7.4's closed set; this build produces "malformed" and
	// "unsupported".
	//
	// IT IS THE FIELD ledger item 236 IS NAMED AFTER. Something IS at this position, this build
	// cannot show it, and without this field that is indistinguishable from a message somebody
	// sent with no text: both are body_len 0. The two are different sentences to a user --
	// "upgrade" and "this could not be read" -- and spec C §5.1 is careful that neither is ever
	// shown for the other.
	Gap string `json:"gap"`

	// REPLY only: the parent's message_id, 32 octets as 64 lower case hex characters, and "" on
	// everything else. THE QUOTED TEXT NEVER TRAVELS -- a reply renders by looking its parent up
	// -- and the parent may be deleted, pruned, or not yet fetched by this device.
	ReplyToId string `json:"reply_to_id"`

	// A TOMBSTONE FROM THIS MESSAGE'S OWN SENDER HAS BEEN APPLIED TO IT. The body is still in
	// body_len and still comes back from urnet_message_list_body: urmessage refuses to decide
	// what a UI does with a deleted line, and the record is on the server either way.
	Deleted bool `json:"deleted"`

	// How many reactions stand on this message, which is the bound on
	// urnet_message_list_reaction_info's second index.
	//
	// IT IS HERE FOR THE SAME REASON body_len IS: a renderer reads one info string per row, and
	// the overwhelmingly common answer is 0, which it can act on without a second call. The two
	// cannot disagree -- both are read off one messageEntry, which is one instant.
	ReactionCount int32 `json:"reaction_count"`
}

// messageReactionInfo is one reaction standing on one message.
//
// ── DECISION: REACTIONS GET THEIR OWN ACCESSORS AND ARE NOT AN ARRAY INSIDE messageInfo ──────
//
// They are a per-message COLLECTION, and this abi already has one shape for a collection: a handle,
// a _count, and an accessor at an index (urnet_message_group_list_count/_at,
// urnet_message_list_count/_info/_body). Two reasons for taking that shape here rather than
// inlining a json array, and the first is the one that decides it:
//
//  1. NOTHING CAPS THE REACTIONS ON ONE MESSAGE. msgrepo ledger item 223 is that item and it is
//     FILED and UNRULED: neither len(effectsOn[target]) nor len(Message.Reactions) has a bound and
//     any member can grow either. An array inlined into messageInfo would make THE METADATA OF ONE
//     ROW unbounded -- a renderer that today frees one small string per row would be handed a
//     string whose size a hostile member chose, on every repaint, for every row. With a count and
//     an index the caller renders the first few and pays for what it asked for. That is the
//     "without allocating unbounded memory up front" half, and it is why this is two exports.
//  2. THE CONSUMER THAT TESTS THIS ABI HAS NO JSON PARSER. ctest/message_abi_test.c reads a field
//     with strstr and a copy up to the next quote, which cannot address the k-th element of an
//     array at all. An array would be a surface this abi's own test could not check.
//
// WHY THE EMOJI IS SAFE IN JSON WHEN A BODY IS NOT, since this looks like an exception to the body
// rule at the top of this file. A body is arbitrary octets from another device and json would
// replace every ill-formed byte with U+FFFD. An emoji is NOT arbitrary: urmessage's checkEmoji
// requires valid UTF-8 of 1..MaxEmojiOctets octets on BOTH paths into a Reaction --
// parseReactionBody on the way in, encodeReaction on the way out -- so encoding/json round-trips it
// byte for byte, and it is bounded, so one reaction's json is bounded with it. The premise is
// MEASURED rather than asserted: TestTheEmojiSurvivesJsonAndAnIllFormedOneWouldNot. If checkEmoji
// ever stops requiring valid UTF-8, this field has to become counted octets like a body.
type messageReactionInfo struct {
	// The reactor's 16 octet sender_handle, lower case hex. IT IS NOT A PERSON: the alpha has no
	// identity system, so two devices of one person are two reactors (open item D7).
	SenderHandle string `json:"sender_handle"`

	// The emoji as that member's device sent it, RAW. It is not folded to §5.3's grouping key --
	// that needs normalisation tables urmessage does not carry (open item M1-41) -- so two
	// spellings of one emoji are two reactions here, and a caller that groups them says so.
	Emoji string `json:"emoji"`

	// True when THIS device sealed the reaction, which is what a UI highlights.
	Mine bool `json:"mine"`
}

// messageGroupRemoval is what urnet_message_group_removal carries: ledger ruling 52's state, under
// the same snake_case rule as everything else on this boundary.
//
// TWO KEYS AND NOT ONE, AND THE SECOND IS NOT DECORATION. `removed` is what a composer is disabled on;
// `removed_epoch` is the LAST epoch this device was a member of, which is the highest one the server
// will serve it (item 246's ceiling) and therefore the exact point the transcript it may still read
// stops at. A screen that says "you are no longer in this group" over a conversation that visibly ends
// somewhere needs to be able to say where.
type messageGroupRemoval struct {
	Removed      bool   `json:"removed"`
	RemovedEpoch uint64 `json:"removed_epoch"`
}

// messageGroupStats is urmessage.Stats under the same snake_case rule.
type messageGroupStats struct {
	Fetched         uint64 `json:"fetched"`
	Opened          uint64 `json:"opened"`
	SkippedCeremony uint64 `json:"skipped_ceremony"`
	SkippedOwn      uint64 `json:"skipped_own"`
	OpenedOwn       uint64 `json:"opened_own"`
	OwnWithoutCopy  uint64 `json:"own_without_copy"`
	SkippedSeen     uint64 `json:"skipped_seen"`
	Unopened        uint64 `json:"unopened"`
	Omitted         uint64 `json:"omitted"`
	SkippedClass    uint64 `json:"skipped_class"`
	// THE DEVICE WRAP THAT CARRIES pq_secret[n+1] (ledger item 251, rulings 37 and 38). Four
	// numbers for four states, three of them failures with a typed sentinel each. The day a wrap
	// carries key material a member that never opens a readable one goes dark in BOTH directions
	// and permanently, with an undiagnosable REASON_REJECTED -- so a caller that can only learn
	// about it by holding an error cannot answer "is this happening to my users". These are what
	// it reads instead.
	//
	// wrap_opened rises by one per epoch change this device did not commit itself, and a zero
	// across a commit is the first thing to look at. wrap_missing is item 132's omission measured
	// at the victim; wrap_unreadable is a wrap at this device's own handle that did not open; and
	// wrap_orphaned is the fan-out of a committer that LOST its CAS race -- a number there with
	// no wrap_missing beside it is the healthy reading, because the winner's own wrap was in the
	// same page and was used.
	//
	// THE ORPHAN DOES NOT REPAIR ITSELF, and this comment used to say it does. That is true only
	// of the reading above, where the winner's wrap arrived; the SENTINEL is reached exactly when
	// it did not, and then this device has followed a commit into an epoch it holds no pq_secret
	// for and is dark in both directions, permanently, across restarts. urmessage.ErrOrphanWrap
	// carries the measurement for why no later page can repair it. All three failures cost the
	// same thing; what the three numbers separate is WHO to go to, not how bad it is.
	//
	// AND ALL FOUR ARE THIS PROCESS'S. The STATE does not reset at a restart -- a group that went
	// dark comes back dark and says so by name -- but these counters do, so a caller that watches
	// them alone sees a healthy-looking device.
	WrapOpened     uint64 `json:"wrap_opened"`
	WrapMissing    uint64 `json:"wrap_missing"`
	WrapUnreadable uint64 `json:"wrap_unreadable"`
	WrapOrphaned   uint64 `json:"wrap_orphaned"`
	// Records that OPENED and became a GAP rather than a message, counted apart because they are
	// two different sentences about the group and only one of them is anybody's fault.
	//
	// THEY ARE ALSO THE ONLY LOUD SIGNAL LEFT FOR A MALFORMED RECORD. Ledger item 224 took a
	// permanent post-open refusal off the fail() path: the record now resolves once, so
	// failed_open does not move, unopened does not move, and urnet_message_group_receive answers
	// no error. A caller that watches only out_error no longer learns that a record could not be
	// read -- gap_malformed and the per-message gap field are what is left to learn it from.
	GapMalformed   uint64 `json:"gap_malformed"`
	GapUnsupported uint64 `json:"gap_unsupported"`
	// Records that became an out_of_window gap: sealed at an epoch no schedule on this device
	// reaches -- more than the past epoch window behind, or before this device was admitted. Since
	// ledger item 241 a member who WAS there produces none of these across a membership change; a
	// later joiner produces one per pre-admission record, which is MLS's own answer for it.
	GapOutOfWindow uint64 `json:"gap_out_of_window"`
	// Records that OPENED under a PRIOR epoch's schedule: sealed at an epoch this device has left
	// and opened anyway, because it was a member then. Ledger item 241. A subset of opened, carried
	// apart so that "history survived the change" is a number a caller can show and not an absence.
	OpenedPastEpoch uint64 `json:"opened_past_epoch"`
	// Records that became a line of the conversation whose sender was an OBSERVER at the epoch it
	// sealed them (ledger item 242's R4, spec C §5.6): a member running a build that does not take
	// the send refusal. OBSERVER is enforced in the client and by the MLS proposal rules and NOT by
	// the server -- an observer holds the group keys and can encrypt a valid application message --
	// so this is the number of times this build had to HIDE one rather than stop it. The records
	// are in the log with their bodies intact; sender_role_at_send is which ones.
	HiddenObserver uint64 `json:"hidden_observer"`
	// Reaction records REFUSED because their sender was an OBSERVER at the epoch it sealed them
	// (item 242's ruling 25). The reaction is not applied at any honest receiver, so it never
	// reaches a message's reactions array and there is nothing for a caller to draw: a message is
	// KEPT and collapsed because dropping it would hide that something was said, and a reaction
	// that is not applied hides nothing, since the message it names is right there whole. This is
	// one per RECORD and not per application. An observer's TOMBSTONE is a different answer and is
	// deliberately NOT counted here: it only ever retracts the observer's own message (ruling 26).
	ObserverReactionRefused uint64 `json:"observer_reaction_refused"`
	// Records that OPENED and whose sender's role at the sending epoch could not be read, so their
	// sender_role_at_send is empty on a row that is otherwise whole. IT MUST STAY ZERO: the role is
	// read off the same handle the open read. It is NOT gap_out_of_window's counterpart -- a record
	// whose epoch no schedule reaches never opens and is never asked about -- so a rising number
	// here is a defect and not a membership change.
	RoleUndeterminable uint64 `json:"role_undeterminable"`
	// Commits this group INGESTED: §6.1 membership-change records this device processed, authorized,
	// applied and followed into the next epoch. One per epoch this device was carried into rather
	// than authored.
	Ingested uint64 `json:"ingested"`
	// Commits this group REFUSED on the receiving-client authorization check (MASTER §11, ledger
	// item 242): processed, judged against the role model, not applied, and the group left at
	// the epoch it was at. A number here is a member that committed what its role does not
	// permit, and a group this device can no longer write to until it is re-founded.
	CommitRefused uint64 `json:"commit_refused"`
	// Commits THIS DEVICE was asked to make and refused before building them: the committing arm
	// of the same rules (item 242's R2). The go verbs that ask -- AddMemberAndPublish, SetRole,
	// TransferOwnership -- answer the refusal as their error, and over this abi
	// urnet_message_group_set_role and _transfer_ownership answer URNET_MESSAGE_COMMIT_REFUSED;
	// this is the number that persists past the call. Nothing moved for anybody, the commit was
	// never built, so it is a request this device's role did not permit and not a halt.
	CommitRefusedOwn uint64 `json:"commit_refused_own"`
	FailedOpen       uint64 `json:"failed_open"`
	Submitted        uint64 `json:"submitted"`
	Rebound          uint64 `json:"rebound"`
	Pages            uint64 `json:"pages"`
	Unattested       uint64 `json:"unattested"`
	// Times this group RAISED the floor of its own durable stream past indices the server already
	// holds claims at under this device's own sender_handle (ledger item 245). It is EXACTLY ZERO
	// on a healthy device for the life of a group; it goes to one on the first walk of a device
	// that was added onto a REMOVED member's leaf, which is the walk that stops that device being
	// refused on its first send and unable to send in that group for the life of the process.
	StreamFloorSeeded uint64 `json:"stream_floor_seeded"`

	// Records given up on (`unopened`) that the server WOULD NOT ATTRIBUTE TO A STREAM: no
	// §4.3.3 sender_handle projection of sixteen octets beside the octets it could not parse. A
	// strict subset of `unopened`, and the only rows about which a device cannot say whether they
	// spent an index on its own stream. It is EXACTLY ZERO against a server that serves §4.3.3
	// rows, which every deployed one does; a number here is a server that is not, and what it
	// costs is the sticky refusal that server could answer any submit with anyway.
	UnopenedUnattributed uint64 `json:"unopened_unattributed"`
}

func messageInfoOf(entry messageEntry) *messageInfo {
	if entry.message == nil {
		return nil
	}
	message := entry.message
	// encoding/hex rather than hand-rolled nibbles. The hand-rolled form was correct, and it
	// tripped connect/message TestClassBucketJoinIsConfinedToRecordGo -- a gate that scans this
	// repository too and forbids splitting a byte as >>4 / &0x0F outside record.go, because that
	// is how a retention-class wire byte is split into its class and its eph bucket. This code
	// was splitting a byte into hex digits: the same SHAPE, an unrelated PROPERTY. Filed against
	// the gate as a false-positive class; the standard library is the better answer either way.
	handle := []byte(hex.EncodeToString(message.SenderHandle))
	return &messageInfo{
		RecordId:         message.RecordId,
		SenderHandle:     string(handle),
		SenderIdentity:   hex.EncodeToString(message.SenderIdentity),
		Mine:             message.Mine,
		SenderRoleAtSend: message.SenderRoleAtSend,
		SentAtMs:         message.SentAtMs,
		BodyLen:          int32(len(message.Text)),
		MessageId:        hex.EncodeToString(message.MessageId),
		Kind:             uint8(message.Kind),
		Gap:              string(message.Gap),
		// EncodeToString of a nil slice is "", which is the "not a reply" answer and is why
		// there is no branch here. A REPLY always carries 32 octets: encodeReply refuses a
		// target of any other width, so a non-empty value is always 64 characters.
		ReplyToId:     hex.EncodeToString(message.ReplyToId),
		Deleted:       entry.deleted,
		ReactionCount: int32(len(entry.reactions)),
	}
}

//export urnet_message_list_count
func urnet_message_list_count(self C.uint64_t) C.int32_t {
	defer cgoGuard("urnet_message_list_count")
	self_, ok := resolveHandle[*messageList](uint64(self), "urnet_message_list_count")
	if !ok || self_ == nil {
		return 0
	}
	return C.int32_t(len(self_.entries))
}

// urnet_message_list_info is one message's metadata as json, WITHOUT the body and WITHOUT its
// reactions. Free with urnet_free_string.
//
//export urnet_message_list_info
func urnet_message_list_info(self C.uint64_t, index C.int32_t) *C.char {
	defer cgoGuard("urnet_message_list_info")
	self_, ok := resolveHandle[*messageList](uint64(self), "urnet_message_list_info")
	if !ok || self_ == nil {
		return nil
	}
	if index < 0 || int(index) >= len(self_.entries) {
		return nil
	}
	return cJson(messageInfoOf(self_.entries[index]), "urnet_message_list_info")
}

// urnet_message_list_body is one message's body, byte for byte, through the buffer-out pattern:
// call once with out == NULL to size it, again to fill it. It is the ONLY way a body leaves this
// abi, for the reason the body decision at the top of this file gives.
//
// A DELETED MESSAGE STILL HAS ITS BODY HERE. urmessage marks the tombstone and keeps the text,
// because it refuses to be the layer that throws away a user's data on a peer's say-so, and the
// record is on the server either way. What a UI does with it is the UI's decision; "deleted" in
// the info json is how it learns there is one to make.
//
//export urnet_message_list_body
func urnet_message_list_body(self C.uint64_t, index C.int32_t, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_message_list_body")
	self_, ok := resolveHandle[*messageList](uint64(self), "urnet_message_list_body")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	if index < 0 || int(index) >= len(self_.entries) {
		return C.bool(false)
	}
	return copyOut(out, inoutLen, []byte(self_.entries[index].message.Text))
}

// ── the reactions standing on one message ───────────────────────────────────────────────────
//
// The collection shape this abi already has, one level down: a count and an accessor at an index.
// See messageReactionInfo for why it is this rather than an array inside urnet_message_list_info,
// and why the emoji may cross inside json when a body may not.
//
// BOTH ANSWER OFF THE SNAPSHOT THE LIST HANDLE TOOK, so a `for k in 0..count` loop cannot be
// overtaken by a reaction landing on another thread. See messageEntry.

// urnet_message_list_reaction_count is how many reactions stand on the message at index. 0 for an
// index out of range and 0 for handle 0, like every other accessor here.
//
//export urnet_message_list_reaction_count
func urnet_message_list_reaction_count(self C.uint64_t, index C.int32_t) C.int32_t {
	defer cgoGuard("urnet_message_list_reaction_count")
	self_, ok := resolveHandle[*messageList](uint64(self), "urnet_message_list_reaction_count")
	if !ok || self_ == nil {
		return 0
	}
	if index < 0 || int(index) >= len(self_.entries) {
		return 0
	}
	return C.int32_t(len(self_.entries[index].reactions))
}

// urnet_message_list_reaction_info is one reaction as json:
// {"sender_handle":"<32 hex>","emoji":"...","mine":bool}. NULL for either index out of range.
// Free with urnet_free_string.
//
//export urnet_message_list_reaction_info
func urnet_message_list_reaction_info(self C.uint64_t, index C.int32_t, reactionIndex C.int32_t) *C.char {
	defer cgoGuard("urnet_message_list_reaction_info")
	self_, ok := resolveHandle[*messageList](uint64(self), "urnet_message_list_reaction_info")
	if !ok || self_ == nil {
		return nil
	}
	if index < 0 || int(index) >= len(self_.entries) {
		return nil
	}
	reactions := self_.entries[index].reactions
	if reactionIndex < 0 || int(reactionIndex) >= len(reactions) {
		return nil
	}
	return cJson(reactionInfoOf(reactions[reactionIndex]), "urnet_message_list_reaction_info")
}

func reactionInfoOf(reaction urmessage.Reaction) *messageReactionInfo {
	return &messageReactionInfo{
		SenderHandle: hex.EncodeToString(reaction.SenderHandle),
		Emoji:        reaction.Emoji,
		Mine:         reaction.Mine,
	}
}

// ── the member list handle ──────────────────────────────────────────────────────────────────
//
// The collection shape this abi has for a page of messages, over the roster: a count and an info
// json at an index. A roster is bounded (1,000 leaves, ruling 7) and carries no body, so a single
// json array would have been safe -- it is a list handle anyway so that the C consumer that tests
// this abi, which reads json with strstr, can address the k-th member, and so that a roster and a
// page of messages are read by one loop shape.

//export urnet_message_member_list_count
func urnet_message_member_list_count(self C.uint64_t) C.int32_t {
	defer cgoGuard("urnet_message_member_list_count")
	self_, ok := resolveHandle[*memberList](uint64(self), "urnet_message_member_list_count")
	if !ok || self_ == nil {
		return 0
	}
	return C.int32_t(len(self_.members))
}

// urnet_message_member_list_info is one member as json:
// {"leaf_index":u32,"sender_handle":"<32 hex>","identity_pub":"<hex>","role":"owner","mine":bool}.
// NULL for an index out of range. Free with urnet_free_string.
//
//export urnet_message_member_list_info
func urnet_message_member_list_info(self C.uint64_t, index C.int32_t) *C.char {
	defer cgoGuard("urnet_message_member_list_info")
	self_, ok := resolveHandle[*memberList](uint64(self), "urnet_message_member_list_info")
	if !ok || self_ == nil {
		return nil
	}
	if index < 0 || int(index) >= len(self_.members) {
		return nil
	}
	return cJson(memberInfoOf(self_.members[index]), "urnet_message_member_list_info")
}
