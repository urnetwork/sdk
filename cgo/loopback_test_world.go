//go:build urnet_message_loopback

package main

/*
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>
*/
import "C"

import (
	"bytes"
	"context"
	"crypto/rand"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/message-server/api"
	"github.com/urnetwork/message-server/peer"
	"github.com/urnetwork/message-server/store"
	"github.com/urnetwork/sdk/urmessage"
)

// A RUNNING MESSAGE SERVER AND ITS CLIENTS, FOR THE C CONSUMER TEST AND FOR NOTHING ELSE.
//
// IT IS NOT IN THE SHIPPING LIBRARY. This file is behind `//go:build urnet_message_loopback`, so
// `go build`, `go vet` and every Makefile target compile it out entirely; only
// ctest/run.sh passes the tag, and it builds a SECOND library into build/ctest/ that nothing
// ships. The measurement that says so is in ctest/run.sh: it counts urnet_message_loopback_*
// in both headers and requires 0 in the shipping one.
//
// WHY IT HAS TO EXIST AT ALL, AND THE REASON IS NOT THE ONE IT USED TO BE. A connect.Client
// receives a frame only through an in-process connect.Route or through a PlatformTransport
// dialling an operator with a minted ByJwt. THIS ABI NOW PRODUCES THE SECOND --
// urnet_message_client_new, which ships -- so the sentence that stood here, "urnet_message_transport_new
// takes a connect client handle and no export in this abi produces one", is no longer true, and it
// is recorded as having been rather than quietly replaced.
//
// WHAT REMAINS TRUE IS THE HALF THIS FILE RESTS ON: there is no operator here to dial and no
// credential to dial one with, so a C-level test routed through the shipping client would reach
// nothing at all. These exports are the in-process half, exactly as sdk/cp3b's world_test.go wires
// it, so that the C-level test of the binding can be a REAL conversation through the REAL server
// rather than a mock of one. Nothing here is a double: peer.Peer dispatches §4.2 frames,
// api.Handler runs §5.1's pipeline, store.MemoryStore holds the rows, and the client half is
// entirely the shipping abi.
//
// WHY IT IS HERE AND NOT IN A MODULE OF ITS OWN. It must be in package main, because the handles
// it hands back have to land in this package's own registry (handles.go) -- a second module is a
// second registry and urnet_release could not reach across. And it must NOT be in cgo/go.mod,
// because a `require github.com/urnetwork/message-server` there would make the whole cgo module
// unbuildable from an sdk checkout that has no message-server beside it, which sdk/test.sh
// already has to skip cp3b for. The dependency lives in loopback.go.mod instead, passed with
// `-modfile`, and cgo/go.mod is untouched.

// loopbackWorld is one server and the clients routed to it.
type loopbackWorld struct {
	ctx     context.Context
	cancel  context.CancelFunc
	server  *connect.Client
	peer    *peer.Peer
	clients []*connect.Client
	shaped  *omittingStore
}

// omittingStore is the ONE way this harness bends the server, and it bends it the way a real
// server can fail: it drops the HIGHEST message record out of a fetch page and touches nothing
// else, so `complete` and `high_water_record_id` stay the real store's own numbers. The client
// then sees a page the server called complete whose high water is above everything it handed
// over -- §4.3.4's records-held-back, which is the one failure the AEAD cannot see -- and
// urmessage answers it with ErrFetchOmitted AND the messages that did arrive.
//
// It exists because that is the only cheap way to make urnet_message_group_receive return a
// non-zero list and a non-NULL out_error at once, and "a caller that reads an error as nothing
// arrived drops real messages" is the most consequential sentence in that export's document. A
// clause nothing can make fail is a clause that defends nothing. It is modelled on
// sdk/cp3b's own fetchDropsMessages shape.
type omittingStore struct {
	store.Store
	mutex sync.Mutex
	on    bool
}

func (self *omittingStore) omit(on bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.on = on
}

func (self *omittingStore) Fetch(ctx context.Context, request *store.FetchRequest) (*store.FetchResult, error) {
	result, err := self.Store.Fetch(ctx, request)
	if err != nil || result == nil {
		return result, err
	}
	self.mutex.Lock()
	on := self.on
	self.mutex.Unlock()
	if !on {
		return result, nil
	}
	highest := -1
	for at, record := range result.Records {
		if record.IsCommit || len(record.ServerAttachment) != 0 {
			continue
		}
		if highest < 0 || result.Records[highest].RecordId < record.RecordId {
			highest = at
		}
	}
	if highest < 0 {
		return result, nil
	}
	kept := make([]*store.Record, 0, len(result.Records)-1)
	kept = append(kept, result.Records[:highest]...)
	kept = append(kept, result.Records[highest+1:]...)
	result.Records = kept
	return result, nil
}

// urnet_message_loopback_world_omit_highest turns the shape above on or off.
//
//export urnet_message_loopback_world_omit_highest
func urnet_message_loopback_world_omit_highest(self C.uint64_t, on C.bool) {
	defer cgoGuard("urnet_message_loopback_world_omit_highest")
	self_, ok := resolveHandle[*loopbackWorld](uint64(self), "urnet_message_loopback_world_omit_highest")
	if !ok || self_ == nil || self_.shaped == nil {
		return
	}
	self_.shaped.omit(bool(on))
}

const loopbackProtocolVersion = 1

//export urnet_message_loopback_world_new
func urnet_message_loopback_world_new(outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_message_loopback_world_new")
	ctx, cancel := context.WithCancel(context.Background())
	server := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(),
		connect.DefaultClientSettings())
	fail := func(err error) C.uint64_t {
		setErrorOut(outError, err)
		server.Close()
		cancel()
		return 0
	}
	connections, err := peer.NewConnections(rand.Reader, time.Now, time.Hour)
	if err != nil {
		return fail(err)
	}
	checks, err := peer.NewChecks(connections, peer.DefaultMaxRequestBytes)
	if err != nil {
		return fail(err)
	}
	shaped := &omittingStore{Store: store.NewMemoryStore(store.DefaultLimits())}
	handler, err := api.New(api.Config{
		Store:       shaped,
		KnownGroups: api.NewMemoryKnownGroups(),
		Front:       checks,
	})
	if err != nil {
		return fail(err)
	}
	served, err := peer.New(peer.Config{
		Client:      server,
		Handler:     handler,
		Connections: connections,
		Checks:      checks,
		Capabilities: &protocol.Capabilities{
			MaxRequestBytes: peer.DefaultMaxRequestBytes,
		},
		ProtocolVersion: loopbackProtocolVersion,
		ServerId:        bytes.Repeat([]byte{0x5A}, 16),
	})
	if err != nil {
		return fail(err)
	}
	return C.uint64_t(newHandle(&loopbackWorld{
		ctx: ctx, cancel: cancel, server: server, peer: served, shaped: shaped,
	}))
}

// urnet_message_loopback_world_server_id is the destination urnet_message_transport_new wants.
// Free with urnet_free_string.
//
//export urnet_message_loopback_world_server_id
func urnet_message_loopback_world_server_id(self C.uint64_t) *C.char {
	defer cgoGuard("urnet_message_loopback_world_server_id")
	self_, ok := resolveHandle[*loopbackWorld](uint64(self), "urnet_message_loopback_world_server_id")
	if !ok || self_ == nil {
		return nil
	}
	return cString(self_.server.ClientId().String())
}

// urnet_message_loopback_world_client stands up one more real connect.Client and routes it to
// the server, both ways. The returned handle is what urnet_message_transport_new takes.
//
//export urnet_message_loopback_world_client
func urnet_message_loopback_world_client(self C.uint64_t) C.uint64_t {
	defer cgoGuard("urnet_message_loopback_world_client")
	self_, ok := resolveHandle[*loopbackWorld](uint64(self), "urnet_message_loopback_world_client")
	if !ok || self_ == nil {
		return 0
	}
	client := connect.NewClient(self_.ctx, connect.NewId(), connect.NewNoContractClientOob(),
		connect.DefaultClientSettings())
	toServer := make(connect.Route)
	toClient := make(connect.Route)
	client.RouteManager().UpdateTransport(connect.NewSendGatewayTransport(), []connect.Route{toServer})
	client.RouteManager().UpdateTransport(connect.NewReceiveGatewayTransport(), []connect.Route{toClient})
	client.ContractManager().AddNoContractPeer(self_.server.ClientId())
	self_.server.RouteManager().UpdateTransport(
		connect.NewSendClientTransport(connect.DestinationId(client.ClientId())), []connect.Route{toClient})
	self_.server.RouteManager().UpdateTransport(
		connect.NewReceiveGatewayTransport(), []connect.Route{toServer})
	self_.server.ContractManager().AddNoContractPeer(client.ClientId())
	self_.clients = append(self_.clients, client)
	return C.uint64_t(newHandle(client))
}

// urnet_message_loopback_world_unrouted_client is a real connect.Client with NO route to
// anything. A transport over it sends Hellos that are never answered, which is what the ~60s
// operator reconnect window looks like from the client, and is what the cancellation case needs
// in order to have something real to cancel.
//
//export urnet_message_loopback_world_unrouted_client
func urnet_message_loopback_world_unrouted_client(self C.uint64_t) C.uint64_t {
	defer cgoGuard("urnet_message_loopback_world_unrouted_client")
	self_, ok := resolveHandle[*loopbackWorld](uint64(self), "urnet_message_loopback_world_unrouted_client")
	if !ok || self_ == nil {
		return 0
	}
	client := connect.NewClient(self_.ctx, connect.NewId(), connect.NewNoContractClientOob(),
		connect.DefaultClientSettings())
	self_.clients = append(self_.clients, client)
	return C.uint64_t(newHandle(client))
}

// ── THE THREE SEND VERBS THAT USED TO BE HERE ARE GONE, AND THAT IS THIS FILE'S OWN PLAN ────
//
// urnet_message_loopback_group_send_reply, _react and _delete stood here so that the READ side of a
// reply, a reaction and a tombstone could be driven from C while no shipping export could produce
// one. Their comment said, in as many words: "THE DAY THE SHIPPING ABI GAINS ITS OWN SEND VERBS
// THESE SHOULD GO, and the C consumer's steps should move onto them unchanged." That day is this
// one. exports_message.go now ships urnet_message_group_send_reply, _react, _unreact and _delete,
// ctest/message_abi_test.c drives those, and keeping a second set behind the tag would mean the
// only C-level exercise of these four verbs ran against code that never ships.
//
// THE ONE THING THAT CHANGED IN THE MOVE is how a target crosses: these took 64 hex characters,
// because hex is what urnet_message_list_info hands a caller. The shipping exports take COUNTED
// OCTETS, which is exports_message.go's rule for every binary value going in -- group_id and
// key_package both -- so the C consumer decodes the hex once, in hex_to_id(), and that decode is
// now part of what the consumer test proves works.

// urnet_message_loopback_gap_list is a message list handle holding one ordinary message and the
// three GAPS this build can produce, so that a C caller can measure that it can tell them apart --
// and, since R4, that sender_role_at_send is non-empty on exactly the rows that OPENED.
//
// THESE THREE ARE BUILT IN GO AND ARE NOT OPENED OFF THE WIRE, WHICH IS STATED RATHER THAN HIDDEN.
// Nothing in this tree can seal a malformed body or an unknown kind from OUTSIDE urmessage: Send
// refuses a plaintext it would not parse back, and the shaped store above corrupts ct_body, which
// fails at the body hash BEFORE the open and is therefore a fail() rather than a gap. That is
// msgrepo ledger item 235, filed and open, and it wants a raw-plaintext seal in the cp3b harness
// first. So the WALK that produces a gap is held where it can be held -- urmessage's own
// TestAnUnknownKindKeepsItsPositionAndIsNotAFailure and the cases beside it, over real records --
// and what crosses HERE is the other half, which is the half ledger item 236 is about: that the
// projection and the list accessors carry a gap to a C caller as something it can tell apart from
// a message.
//
//export urnet_message_loopback_gap_list
func urnet_message_loopback_gap_list() C.uint64_t {
	defer cgoGuard("urnet_message_loopback_gap_list")
	handle := bytes.Repeat([]byte{0x3C}, 16)
	return C.uint64_t(newMessageList([]*urmessage.Message{
		{
			RecordId: 41, SenderHandle: handle, SentAtMs: 1,
			MessageId: bytes.Repeat([]byte{0x01}, 32),
			Kind:      urmessage.KindText, Text: "a message that is a message",
			// THE THREE ROLES BELOW ARE THREE DIFFERENT VALUES ON PURPOSE. A projection that
			// carried sender_role_at_send as one constant would satisfy any single-row check,
			// and the C consumer reads all four rows.
			SenderRoleAtSend: "owner",
		},
		{
			// A CODE THIS BUILD DOES NOT KNOW, on a class its range allows: the record opened
			// and its signature verified, so the sender did nothing wrong. 0x03 is ATTACHMENT,
			// which is assigned and has no body in this build.
			RecordId: 42, SenderHandle: handle, SentAtMs: 2,
			MessageId: bytes.Repeat([]byte{0x02}, 32),
			Kind:      urmessage.KindAttachment, Gap: urmessage.GapUnsupported,
			// A GAP THAT OPENED STILL KNOWS WHO SENT IT AND WHAT ROLE THEY HELD.
			SenderRoleAtSend: "member",
		},
		{
			// A SENDER THAT BROKE A RULE ALREADY WRITTEN, and the kind is the code the record
			// ARRIVED under rather than what the record is: a malformed REPLY is still a REPLY
			// here, and a caller that branched on kind would draw an empty reply.
			RecordId: 43, SenderHandle: handle, SentAtMs: 3,
			MessageId: bytes.Repeat([]byte{0x03}, 32),
			Kind:      urmessage.KindReply, Gap: urmessage.GapMalformed,
			// AND A MALFORMED RECORD FROM AN OBSERVER IS BOTH THINGS AT ONCE: a gap a UI draws
			// as a closed placeholder, and a record whose sender may not send.
			SenderRoleAtSend: "observer",
		},
		{
			// THE ONE REASON THIS BUILD PRODUCES THAT CARRIES NO ROLE, and the reason the field
			// is not simply always filled: an out_of_window record NEVER OPENED -- it was sealed
			// at an epoch no schedule on this device reaches -- so nothing on this device can say
			// what role its sender held then, and the handle it names is an unauthenticated
			// claim. Non-empty IFF the record opened; this is the "only if" half, at the
			// boundary. It also carries no kind, because no body was read.
			RecordId: 44, SenderHandle: handle, SentAtMs: 0,
			MessageId:        bytes.Repeat([]byte{0x04}, 32),
			Gap:              urmessage.GapOutOfWindow,
			SenderRoleAtSend: "",
		},
	}))
}

//export urnet_message_loopback_world_close
func urnet_message_loopback_world_close(self C.uint64_t) {
	defer cgoGuard("urnet_message_loopback_world_close")
	self_, ok := resolveHandle[*loopbackWorld](uint64(self), "urnet_message_loopback_world_close")
	if !ok || self_ == nil {
		return
	}
	for _, client := range self_.clients {
		client.Close()
	}
	self_.clients = nil
	self_.peer.Close()
	self_.server.Close()
	self_.cancel()
}
