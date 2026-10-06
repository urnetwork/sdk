//go:build !sdk_mobile_bind

package sdk

import (
	"errors"
)

// The typed refusals the messaging client's durable stream store owns.
//
// Every one of them is a section 5.9 G7 fatal error: a value a caller matches with errors.Is,
// never a bool, never a log line, and -- this is the whole reason the file exists -- never
// (0, nil). Contract clause 4 of connect/messagegroup.StreamIndexReserver makes an unseen
// stream an error-free zero, and a sender ratchet resumes at HighWater + 1. So every condition
// that is NOT "this stream was never seen" and is answered as a zero anyway restarts the
// ladder at index 1 under a class key that has not moved, which spec A section 5.6 calls
//
//	a total break of both AEADs for that record.
//
// The errors below are the conditions that must never take that exit.
var (
	// ErrStreamKeyWidth is a group id or a sender handle offered to the store at a width
	// connect/messagegroup.StreamKey cannot hold, or a count of key parameters that is not
	// the count that type declares. Section 8.2 spells the two parameters []byte and states
	// no length rule; StreamKey's fields are fixed-width arrays and are total over their
	// domain. The flattening between the two is the only place a 17-octet group id can
	// become a panic or a silent truncation, and a truncation collides two streams onto one
	// row -- which hands the second stream indices the first has already used. The refusal
	// names which parameter it refused and what width it had.
	ErrStreamKeyWidth = errors.New("stream key width")

	// ErrStreamKeySpace is a row written under a key derivation this build does not produce.
	//
	// THIS IS LEDGER ITEM 170's MECHANISM. Ruling A1 removed the retention-class byte from
	// StreamKey, and StreamKey's field set IS the identity a reserver keys a row on. A store
	// holding rows under the older, three-field derivation answers HighWater 0 for an A1 key
	// -- silently, because clause 4 makes "never seen" an error-free zero -- and the ladder
	// restarts at 1. The store therefore versions its whole key space by a tag derived from
	// StreamKey's own field set and REFUSES any row bearing a different one, rather than
	// migrating rows that, on the day this shipped, did not exist anywhere.
	//
	// The refusal is deliberately coarse: one foreign-tagged row refuses every key in the
	// directory, because a directory written by another build tells this build nothing about
	// which of its keys that build had already spent.
	ErrStreamKeySpace = errors.New("stream key space")

	// ErrStreamStoreState is state the store found and cannot answer from: an entry in the
	// row directory that is not a row under any tag, a row whose records are damaged in a
	// way an interrupted append cannot produce, an unreadable row directory, or a store that
	// has been closed.
	//
	// It is explicitly NOT the answer for a torn tail. A trailing record that does not verify
	// is the ORDINARY outcome of a crash mid-append; refusing it would leave a row no later
	// process could open, on exactly the path the durability exists to survive. See
	// classifyStreamRow for the three cases and the discriminator between them.
	//
	// IT IS A MIXED CLASS AND THE ADAPTER RULES THE WHOLE OF IT TRANSIENT, so a condition
	// inside it that a retry can never clear retries forever. ONE sub-class carries its own
	// discriminator rather than waiting on that ruling: A ROW THIS STORE HAS READ AND COULD NOT
	// CLASSIFY is raised with ErrStreamStoreConsumed beside this value, by persistedHighWater,
	// because nothing in this store ever rewrites a row it refused. That is keyed on the
	// CONDITION and not on when the store found out -- at open, or one call later under a live
	// store, it is the same bytes and the same refusal -- and the complement of it inside the
	// same class is the row whose octets could NOT BE OBTAINED: an os.Open or a ReadAt that
	// failed is a condition a retry can clear and stays this value alone. The REST of the class --
	// a failed flush, a full disk, an unreadable row directory, a closed store -- is still
	// ruled transient and is still, for the permanent members of it, an unbounded retry.
	// That remainder is FILED, not ruled here: see streamStoreSentinelRulings.
	ErrStreamStoreState = errors.New("stream store state")

	// ErrStreamStoreRewound is PERSISTED STATE BEHIND AN INDEX THIS STORE HAS ALREADY HANDED
	// OUT. It is contract clause 2's condition -- "HighWater never rewinds. After a restart it
	// is at least what it was, for every key, under every interleaving" -- and it is raised by
	// BOTH stream methods, because a caller that only ever queried is still owed the answer
	// that the number it saw is no longer on the disk.
	//
	// The condition is sdk's; the NAME messagegroup branches on is the adapter's, and the
	// adapter is the only code that maps one onto the other. Nothing in this package imports
	// messagegroup.ErrStreamIndexRewound.
	//
	// It is raised against WHAT THIS PROCESS HAS CONFIRMED ON THE DISK -- the verified prefix
	// of the row, seeded by the open-time scan and grown by every read and every allocation --
	// and never against a number a caller remembers: a store that took the caller's word for
	// its own high water would be a store whose rewind detector a caller can turn off. The
	// confirmed prefix is strictly wider than the set of indices this store has RETURNED,
	// which is what an earlier version compared against: a row this process has only ever READ
	// is still a row it can catch going backwards, and every row the open-time scan found is
	// in exactly that state.
	//
	// THE TWO OBSERVABLES are a row that is ABSENT after this store confirmed part of it, and
	// a row SHORTER than the prefix this store confirmed. Both are refused by
	// persistedHighWater, before anything opens the row for writing -- which is what makes the
	// refusal stick instead of recreating, with os.O_CREATE, the very row it refused.
	ErrStreamStoreRewound = errors.New("stream store rewound")

	// ErrStreamStoreConsumed is the store's PERMANENT refusal to allocate for a key: the next
	// position is one it has already handed out and it has no way past it. Contract clause 3
	// names two shapes of that and this store produces both, and a THIRD below is the store's
	// own rather than a reading of the clause.
	//
	//  1. A stream that has spent the last index a u64 holds. persisted+1 does not exist, and
	//     no later call can make it exist, so the refusal is forever.
	//  2. A row that went backwards under a live process. The next position this store would
	//     allocate is a number it has already returned to a caller, and a second AEAD record
	//     under a reused stream_index is what spec A section 5.6 calls "a total break of both
	//     AEADs for that record". Shape 2 is raised TOGETHER WITH ErrStreamStoreRewound, both
	//     findable by errors.Is off one value, because the two names describe one state from
	//     two seats: the reader's (the number moved) and the allocator's (I cannot go on).
	//     streamindex.go's clause 3 says exactly this -- the permanent refusal is "what a row
	//     that went backwards under a live process looks like from in here".
	//
	//  3. A row THIS STORE HAS READ AND COULD NOT CLASSIFY -- found so by the open-time scan
	//     or by a live read, which are the same condition met at two times. The indices it has
	//     already spent are not derivable from it, so no next position can be proven unspent,
	//     and no later call in this store's life changes that, because nothing in this store
	//     ever rewrites a row it refused. Its complement inside the same class is the row whose
	//     octets could not be OBTAINED, which stays transient. Shape 3 is
	//     raised TOGETHER WITH ErrStreamStoreState, both findable by errors.Is off one value,
	//     for the same reason shape 2 is raised with ErrStreamStoreRewound: one state, two
	//     seats. It is the store supplying a discriminator the adapter cannot invent, and it
	//     is the only part of the ErrStreamStoreState class whose permanence is knowable from
	//     inside the store.
	//
	// It is never the answer for a condition a retry could clear. A transient filesystem error
	// is returned as itself; calling it consumed would tell a ratchet to stop forever over a
	// full disk.
	ErrStreamStoreConsumed = errors.New("stream store consumed")

	// ErrStreamStoreLocked is another StreamStore holding this directory.
	//
	// Two stores over one directory each read the same persisted high water and each allocate
	// THE SAME NEXT INDEX -- section 5.6's total break, reached without a single corrupt byte.
	// The exclusion that prevents it is held by the OPERATING SYSTEM and released by the death
	// of the process that held it: dwShareMode=0 on Windows, LOCK_EX|LOCK_NB on the GOOS set
	// where syscall.Flock is declared. There is deliberately NO liveness heuristic here -- no
	// pid, no timestamp, no age threshold -- because a heuristic either wedges a directory
	// forever after a crash or steals a lock from a live writer, and the SDK cannot tell those
	// two apart.
	//
	// A GOOS with neither primitive gets this error unconditionally. A platform this store
	// cannot make safe is a platform it refuses to open on; a build tag that quietly compiled
	// to a no-op would be the single-writer property deleted by a build constraint.
	ErrStreamStoreLocked = errors.New("stream store locked")
)
