//go:build !sdk_mobile_bind

package sdk

// THE MESSAGING CLIENT.
//
// This file carries no code. It carries the three things the next person to write a call on
// this leg needs before they write it, in the source rather than only in a plan document.
//
// ---------------------------------------------------------------------------------------------
// READ EVERY SIGNATURE FROM THE FILE THAT DECLARES IT. THE SOURCE COMMENT ON THIS LEG'S OWN
// SUBJECT IS THE STALE ONE.
// ---------------------------------------------------------------------------------------------
//
// Spec A section 8.2's stream-index pair is stated THREE different ways by three live documents,
// and no two of them agree. All three were read on 2026-09-12 before this file was written.
//
//  1. connect/messagegroup/streamindex.go's package comment quotes section 8.2 as declaring
//
//     ReserveStreamIndex(groupId []byte, index uint64) error
//     StreamHighWater(groupId []byte) (uint64, error)
//
//     That is the PRE-A1 shape: the caller chooses the index and the store says yes or no. The
//     same file's own body then says, at length, that the assert shape wedges permanently under
//     ruling A1, because two ladders sharing one counter both offer index 1 and the second is
//     refused forever. The comment is stale against the file it sits on.
//
//  2. Spec A section 8.2, as amended 2026-09-07, reads
//
//     ReserveStreamIndex(groupId, senderHandle []byte) (uint64, error)
//     StreamHighWater(groupId, senderHandle []byte) (uint64, error)
//
//     and says explicitly that "the flattening from these two []byte parameters to its
//     comparable StreamKey is the implementer's". This is the current and correct statement.
//
//  3. The shipped Go interface is a third thing again:
//
//     Reserve(stream StreamKey) (uint64, error)
//     HighWater(stream StreamKey) (uint64, error)
//
// So the correspondence is NOT parameter for parameter, an adapter between (2) and (3) is
// mandatory, and this package owns it. streamKeyFromOctets below is the flattening, and it is
// the only place in this package where a []byte becomes a StreamKey.
//
// The divergence is filed as S2-15. A one-line correction is owed to the connect comment and it
// is not this repository's to make.
//
// ---------------------------------------------------------------------------------------------
// WHAT THE DURABLE STREAM STORE IS FOR, IN ONE PARAGRAPH.
// ---------------------------------------------------------------------------------------------
//
// Spec A section 5.6: stream_index is a single u64 counter per (group_id, sender_handle),
// write-once, assigned locally. A device MUST durably record "index k consumed" BEFORE
// encrypting. A reused index is a reused nonce under a reused record_key, which is "a total
// break of both AEADs for that record". The store in message_stream_store.go is the thing that
// makes the second time an index is handed out impossible rather than unlikely; every refusal it
// owns is in message_errors.go and every one of them exists because its alternative is a silent
// zero.
//
// ---------------------------------------------------------------------------------------------
// WHY EVERY message_* FILE IS BUILT WITHOUT sdk_mobile_bind.
// ---------------------------------------------------------------------------------------------
//
// The mobile SDK is package sdk run through gobind, and build/cmd/mobileexports runs gobind with
// -tags=sdk_mobile_bind and fails on every declaration gobind skips that its policy does not
// name. This leg's Go surface is full of what gobind skips: connect.Id, funcs, contexts,
// []string, time.Duration. An app reaches the messenger through the C ABI
// (cgo/include/urnetwork_message.h), not through gomobile. So every production message_* file
// carries !sdk_mobile_bind, and the mobile SDK holds none of it, rather than a half-bound
// MessageClient. A new message_* file needs the same constraint, or that check fails naming
// what it skipped.
