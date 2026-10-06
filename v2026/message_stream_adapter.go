//go:build !sdk_mobile_bind

package sdk

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/urnetwork/connect/v2026/messagegroup"
)

// The adapter that presents the durable stream store as the sink a sender ratchet allocates from.
//
// It owns exactly two things and nothing else owns either of them.
//
// THE FLATTENING. Spec A section 8.2 declares the store's pair as ReserveStreamIndex(groupId,
// senderHandle []byte) and StreamHighWater(groupId, senderHandle []byte); the Go interface
// connect/messagegroup declares is Reserve(stream StreamKey) and HighWater(stream StreamKey). The
// method names differ AND the parameter shapes differ, so *StreamStore deliberately does not
// satisfy that interface -- TestStreamStoreDoesNotYetSatisfyStreamIndexReserver holds that as a
// checked fact -- and section 8.2's A1 paragraph makes the flattening between the two "the
// implementer's". This is it, and a second one anywhere in package sdk is the defect this file
// exists to prevent: two flattenings are two derivations of which row a stream's indices land in,
// and the day they disagree the second stream is handed indices the first has already spent.
// TestEveryStreamKeyFlatteningInPackageSdkIsTheAdapters is the gate over that, and it reads the
// package's syntax tree rather than this file.
//
// THE FLATTENING IS REFLECTIVE, AND THAT IS NOT STYLE. No production source in package sdk may
// spell a StreamKey field name -- TestNoProductionSourceOfPackageSdkSpellsAStreamKeyFieldName
// holds it -- because a field added or removed in connect must arrive here as a refusal or a
// changed key space rather than as a field nobody copied. So the octets come off the key's own
// field set in declaration order, which is the same order streamKeyFromOctets puts them back in,
// and the two are inverses by construction rather than by agreement.
//
// THE SENTINEL MAPPING, and it is load-bearing rather than tidy. SenderRatchet.Next branches on
// errors.Is(err, messagegroup.ErrStreamIndexConsumed) to tell a PERMANENT wedge from a TRANSIENT
// failure, and the two want opposite answers: a full disk is a retry and the ladder does not
// move, while a store that cannot allocate will never start being able to, so a ratchet that went
// on asking would refuse every send forever while paying a durable write per attempt. None of the
// store's own sentinels is findable as either messagegroup name --
// errors.Is(ErrStreamStoreConsumed, messagegroup.ErrStreamIndexConsumed) is false -- so an adapter
// that simply forwarded a store error would turn the store's PERMANENT refusal into a transient
// one and wedge a ladder into an unbounded durable-write retry loop. The mapping below is what
// stops that, and TestTheStoreSentinelClassIsTotalOverTheAdaptersMapping is what stops a sentinel
// added later from passing through unclassified.
//
// NewStreamIndexReserver answers a nil interface for a nil store, deliberately. The refusal that
// owes is NewGroupSession's ErrNilStreamIndexReserver, which already exists and already gates
// every seal; a second refusal here would be a second answer to one question.
func NewStreamIndexReserver(store *StreamStore) messagegroup.StreamIndexReserver {
	if store == nil {
		return nil
	}
	return &streamIndexReserver{store: store}
}

type streamIndexReserver struct {
	store *StreamStore
}

// Reserve is contract clause 1 and clause 5: it allocates the next index of this stream and
// returns only after that reservation is durable, and two calls are two indices.
func (self *streamIndexReserver) Reserve(stream messagegroup.StreamKey) (uint64, error) {
	parts, err := self.keyOctets(stream)
	if err != nil {
		return 0, self.classify(err)
	}
	index, err := self.store.ReserveStreamIndex(parts[0], parts[1])
	if err != nil {
		return 0, self.classify(err)
	}
	return index, nil
}

// HighWater is contract clause 2 and clause 4: the highest index the store has ever allocated for
// this stream, or 0 with no error for a stream it has never seen.
func (self *streamIndexReserver) HighWater(stream messagegroup.StreamKey) (uint64, error) {
	parts, err := self.keyOctets(stream)
	if err != nil {
		return 0, self.classify(err)
	}
	highWater, err := self.store.StreamHighWater(parts[0], parts[1])
	if err != nil {
		return 0, self.classify(err)
	}
	return highWater, nil
}

// SeedTo raises this stream's floor to `floor` without allocating, and answers the high water the
// row carries afterwards. See [StreamStore.SeedStreamIndex] for what a seed is and why ledger item
// 245 needs one.
//
// IT IS NOT PART OF messagegroup.StreamIndexReserver AND IT MUST NOT BE. That interface is the
// surface a SENDER RATCHET allocates through, and a ratchet has no business moving a floor; the
// one caller is urmessage's receiving walk, which reaches it through an optional interface of its
// own and goes on working against a reserver that does not have it. The same flattening and the
// same sentinel mapping are used, because a second reading of either is this file's own header's
// defect.
func (self *streamIndexReserver) SeedTo(stream messagegroup.StreamKey, floor uint64) (uint64, error) {
	parts, err := self.keyOctets(stream)
	if err != nil {
		return 0, self.classify(err)
	}
	highWater, err := self.store.SeedStreamIndex(parts[0], parts[1], floor)
	if err != nil {
		return 0, self.classify(err)
	}
	return highWater, nil
}

// keyOctets is THE flattening: a StreamKey onto section 8.2's positional []byte parameters, in
// the key type's own declaration order.
//
// EVERY PART IS A COPY, AND THE COPY IS NOW THE ONLY CLAUSE THAT CARRIES THAT. StreamKey is
// comparable "deliberately twice over" -- it can be a map key without a second encoding, and a
// group id that moved under a ratchet cannot reserve indices against one row and use them against
// another -- and section 8.2's []byte pair is neither comparable nor immutable. Handing the store
// a slice that aliases anything this adapter keeps would put a row's identity in a buffer
// somebody else can write to, and a row's identity is which indices a stream has already spent.
//
// IT USED TO BE TWO CLAUSES AND ONLY ONE OF THEM WAS DRIVEN, which is why this reads the way it
// does now. The earlier version built an ADDRESSABLE image of the key with reflect.New -- the
// language forces one, because Value.Slice and Value.Bytes refuse a non-addressable array -- then
// sliced each field out of that image and copied the slice. The image was fresh per call, so the
// image ALREADY made two calls disjoint, and `append([]byte(nil), octets...)` on top of it was
// unobservable: delete it and each part aliases a heap object this call created and nothing else
// references, so no caller, no test and no mutation of the store could tell. The case named
// TestTheAdapterCopiesTheKeyAtTheBoundary drove the IMAGE and not the copy, and its name said
// otherwise.
//
// reflect.Copy does not require an addressable source, so the image is gone and there is exactly
// one clause left: a destination made per field per call, and the copy into it. Hoist that
// destination onto the receiver, or out of the loop, and two calls or two parts alias -- which is
// precisely what that case asserts, so it now drives the clause its name claims.
//
// reflect.Copy's return is deliberately not branched on. It is min(len(destination), source
// length), the destination is made at exactly field.Type.Len() and the source IS that field, so
// the two are equal by construction: a check on it would be a clause nothing could ever drive,
// which is the defect this round was sent to stop adding.
func (self *streamIndexReserver) keyOctets(stream messagegroup.StreamKey) ([][]byte, error) {
	keyType := streamKeyType()
	key := reflect.ValueOf(stream)
	parts := [][]byte{}
	for i := range keyType.NumField() {
		field := keyType.Field(i)
		if field.Type.Kind() != reflect.Array || field.Type.Elem().Kind() != reflect.Uint8 {
			return nil, fmt.Errorf(
				"%w: field %d of %s is %s and holds no octets, so it cannot become one of the store's key parameters",
				ErrStreamKeyWidth,
				i,
				keyType.String(),
				field.Type.String(),
			)
		}
		// THE COPY. A fresh destination per field per call, and the octets moved into it:
		// nothing this adapter keeps, and nothing two calls share, backs what the store is
		// handed.
		part := make([]byte, field.Type.Len())
		reflect.Copy(reflect.ValueOf(part), key.Field(i))
		parts = append(parts, part)
	}
	if err := self.refuseWrongWidth(parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// refuseWrongWidth is the width boundary AT THE ADAPTER, re-checked here rather than left to the
// store.
//
// The StreamKey direction is total by construction -- the fields are fixed-width arrays -- so on
// a correct build this refuses nothing. It is here because the adapter is the ENTRY point and not
// only the exit: a flattening that truncated a field, or one written against a key type whose
// field count no longer matches the number of parameters the store takes, would otherwise reach
// the store as two well-formed slices naming a row that is not this stream's. Both counts are
// read rather than written down -- the key type's field count through reflection, the store's
// parameter count off the method's own type -- so a field added in connect arrives as this
// refusal instead of as an index out of range.
func (self *streamIndexReserver) refuseWrongWidth(parts [][]byte) error {
	keyType := streamKeyType()
	takes := reflect.TypeOf(self.store.ReserveStreamIndex).NumIn()
	if len(parts) != takes {
		return fmt.Errorf(
			"%w: the flattening produced %d key parameter(s) and the store's allocator takes %d",
			ErrStreamKeyWidth,
			len(parts),
			takes,
		)
	}
	if len(parts) != keyType.NumField() {
		return fmt.Errorf(
			"%w: the flattening produced %d key parameter(s) and %s declares %d fields",
			ErrStreamKeyWidth,
			len(parts),
			keyType.String(),
			keyType.NumField(),
		)
	}
	for i, part := range parts {
		field := keyType.Field(i)
		want := field.Type.Len()
		if len(part) != want {
			return fmt.Errorf(
				"%w: key parameter %d (%s.%s) is %d octets, want exactly %d; a short key padded or a long key truncated collides two streams onto one row, which hands the second indices the first has already used",
				ErrStreamKeyWidth,
				i,
				keyType.String(),
				field.Name,
				len(part),
				want,
			)
		}
	}
	return nil
}

// streamStoreSentinelRuling is one store sentinel and the ONE answer this adapter gives for it.
//
// permanent is the whole of what SenderRatchet.Next reads: true wedges the ladder forever, false
// leaves it to retry. rewound is orthogonal and additive -- it is the reader's name for the same
// state the allocator calls permanent, and the store raises both together on an allocation
// against a row that went backwards, so an error can be both.
//
// ruling is not decoration. It is the sentence a later reader needs when they meet the sentinel
// and disagree with the verdict, and the totality gate prints it.
type streamStoreSentinelRuling struct {
	name      string
	sentinel  error
	permanent bool
	rewound   bool
	ruling    string
}

// streamStoreSentinelRulings is TOTAL over the sentinels package sdk declares, and
// TestTheStoreSentinelClassIsTotalOverTheAdaptersMapping derives that class from the package's
// syntax tree rather than from this list. A sentinel added to the package and not added here
// fails that test; it does not quietly fall through to the transient bucket.
//
// ErrStreamStoreState IS A MIXED CLASS AND IT TAKES ONE ANSWER, which is a ruling rather than a
// classification, and it is written down as one. The store raises it for a failed flush, a full
// disk, a corrupt row and a closed store alike, and an adapter holding only the error value
// cannot tell the first two from the last two -- there is no discriminator to read. Of the two
// mis-mappings the ruling has to choose between, calling a transient failure permanent wedges a
// healthy ladder forever over a full disk, which connect/messagegroup's own ratchet names as the
// case that must stay a retry; calling a permanent one transient costs an unbounded retry loop
// that pays a durable write per attempt against a row that will never accept one. This takes the
// first cost. Splitting the class is the store's repair and not the adapter's: an adapter cannot
// invent a discriminator the value does not carry.
//
// AND THE STORE HAS NOW SUPPLIED ONE, FOR EXACTLY ONE SUB-CLASS. A row THE STORE HAS READ AND
// COULD NOT CLASSIFY is raised as ErrStreamStoreState WITH ErrStreamStoreConsumed beside it, so
// classify's permanent||... finds it and the ladder stops. That is the sub-class whose permanence
// is knowable inside the store: nothing in the store ever rewrites a row it refused, so a body it
// could not classify it will go on being unable to classify.
//
// AND THAT SUB-CLASS IS KEYED ON THE CONDITION, NOT ON THE CLOCK, which it was not until
// 2026-09-12. The discriminator used to be "present in the row directory when the store OPENED",
// so the same octets in the same directory reaching persistedHighWater one call later under a
// live store arrived here bare and were forwarded as transient -- the unbounded retry this file
// exists to stop, reached by nothing more than the order in which the store looked. The line now
// runs between OCTETS OBTAINED AND REFUSED, which is permanent, and OCTETS NOT OBTAINED -- an
// os.Open or a ReadAt that failed -- which stays a retry and stays in the filed remainder below.
// It required no change to the ruling below, which is the point -- the store widened what it
// says, the adapter went on reading it.
//
// WHAT IS STILL OPEN, AND IT IS FILED RATHER THAN RULED HERE. The remainder of the class -- a
// failed flush, a full disk, an unreadable row directory, a CLOSED store -- is still ruled
// transient, and two of those are not conditions a retry clears either. A closed store answers
// ErrStreamStoreState forever, and a ratchet told to retry will ask it forever. The judgement
// that trades a wedged healthy ladder against an unbounded retry loop is the OWNER's -- it is a
// §8.2 contract question about what the store owes a ratchet, not an implementation choice -- and
// the shape of the repair is the store's, not this file's: give each remaining permanent member
// its own discriminator, the way the unreadable row just got one, rather than flipping the
// verdict for the whole class. It is filed here, in the commit that added the sub-class above,
// and in this pass's report; SPEC-LEDGER.md lives in a repository this pass must not write to.
// TestTheStateClassStillRetriesForeverForTheMembersThatAreNotDiscriminated is that residual,
// executable, so it is a measured open item and not a sentence.
var streamStoreSentinelRulings = []streamStoreSentinelRuling{
	{
		name:      "ErrStreamKeyWidth",
		sentinel:  ErrStreamKeyWidth,
		permanent: true,
		ruling:    "a key of the wrong width is the wrong width on every retry, and the stream it names is not a stream this device can allocate for at all",
	},
	{
		name:      "ErrStreamKeySpace",
		sentinel:  ErrStreamKeySpace,
		permanent: true,
		ruling:    "a row written under a key derivation this build does not produce refuses every key in the directory, and no retry rewrites it",
	},
	{
		name:      "ErrStreamStoreState",
		sentinel:  ErrStreamStoreState,
		permanent: false,
		ruling:    "the mixed class, ruled transient: a failed flush and a full disk must stay a retry, and the value carries no discriminator that would separate them from a closed store. The one member that HAS a discriminator -- a row this store has READ AND COULD NOT CLASSIFY, whenever it found that out -- is raised with ErrStreamStoreConsumed beside it by the store, so it reaches this mapping as permanent without the verdict here moving. The rest is an open item filed for the owner, not ruled here",
	},
	{
		name:     "ErrStreamStoreRewound",
		sentinel: ErrStreamStoreRewound,
		rewound:  true,
		ruling:   "persisted state behind what the store confirmed. On its own it is the READER's seat -- StreamHighWater raises it alone -- and the allocator's permanence arrives as ErrStreamStoreConsumed beside it, so this row does not claim permanence of its own",
	},
	{
		name:      "ErrStreamStoreConsumed",
		sentinel:  ErrStreamStoreConsumed,
		permanent: true,
		ruling:    "the store's own word for a refusal to allocate that no later call can clear, which is exactly what messagegroup.ErrStreamIndexConsumed names",
	},
	{
		name:      "ErrStreamStoreLocked",
		sentinel:  ErrStreamStoreLocked,
		permanent: true,
		ruling:    "another process holds the single-writer exclusion. UNREACHABLE THROUGH THIS ADAPTER and measured so by TestTheTwoRulingsNoErrorChainReaches: OpenStreamStore raises it, and a reserver is built from a store that is already open, so the verdict is a placeholder the totality of the class requires rather than an answer anything consults",
	},
	{
		name:      "errStreamAppendInterrupted",
		sentinel:  errStreamAppendInterrupted,
		permanent: false,
		ruling:    "the injected synthetic death. The next allocation overwrites the torn tail in place and succeeds, so it is a retry -- and holding a verdict for it keeps the class total rather than exempting the unexported half of it",
	},
	{
		name:      "errStreamInjectedFlushFailure",
		sentinel:  errStreamInjectedFlushFailure,
		permanent: false,
		ruling:    "the injected synthetic flush failure. UNREACHABLE BY errors.Is and measured so by TestTheTwoRulingsNoErrorChainReaches: writeOneRecord formats it with %v and not %w, so it never enters an error chain and no classification can see it. The verdict is what a real flush failure takes, and it is a placeholder the totality of the class requires rather than an answer anything consults",
	},
}

// classify is the ONE place a store failure becomes a messagegroup sentinel.
//
// It is additive and it never replaces: the store's error is wrapped, so errors.Is still finds
// the store's own sentinel and the store's own message is still in the text. What is added is the
// name SenderRatchet.Next branches on, and nothing else.
//
// An error carrying NO sentinel of the class is forwarded as TRANSIENT. That is the safe half of
// the ruling above and it is safe only because the class is total: a permanent refusal cannot
// reach this bucket without first getting past the gate that holds every sentinel the package
// declares to a verdict.
func (self *streamIndexReserver) classify(err error) error {
	if err == nil {
		return nil
	}
	permanent, rewound := false, false
	for _, ruling := range streamStoreSentinelRulings {
		if errors.Is(err, ruling.sentinel) {
			permanent = permanent || ruling.permanent
			rewound = rewound || ruling.rewound
		}
	}
	switch {
	case permanent && rewound:
		return fmt.Errorf(
			"sdk: the durable stream store refused permanently, and its persisted state went backwards: %w (%w, %w)",
			err,
			messagegroup.ErrStreamIndexConsumed,
			messagegroup.ErrStreamIndexRewound,
		)
	case permanent:
		return fmt.Errorf(
			"sdk: the durable stream store refused permanently, so this ladder cannot go on: %w (%w)",
			err,
			messagegroup.ErrStreamIndexConsumed,
		)
	case rewound:
		return fmt.Errorf(
			"sdk: the durable stream store's persisted state went backwards: %w (%w)",
			err,
			messagegroup.ErrStreamIndexRewound,
		)
	}
	return fmt.Errorf(
		"sdk: the durable stream store could not answer, and a later call may: %w",
		err,
	)
}
