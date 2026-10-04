package urmessage

import (
	"bytes"
	"fmt"

	"github.com/urnetwork/connect/mls"
)

// ForgetGroup is THIS DEVICE'S HALF OF LEAVING A GROUP, and it is the only half a device can do by
// itself. The other half, taking its leaf out of the tree, is a [Group.RemoveMember] some admin or
// the owner makes: no identity's last leaf ever leaves in its own commit (msgrepo ledger 257, ruling
// 48, which also makes a leave PRODUCT SURFACE and not an MLS proposal). The owner's ruling of
// 2026-10-02 asked for "one party can just locally delete and leave"; this is that, built as ledger
// item 273 decided it.
//
// WHAT IT ERASES, IN THIS ORDER. A durable mark goes down first, saying this group is being left.
// Then the group is closed, which erases its pq_secret table and its staged wrap candidates and
// closes the MLS session; from then on every call that reaches the server or the disk refuses, and
// nothing it does writes the disk (Messages, Epoch and Stats still answer, from memory). Then it
// leaves this device's map, so [Device.Groups] stops naming it. Then
// [DeviceStore.DeleteGroupRecord] takes every epoch state (this device's leaf private key and its
// whole path-secret ladder), every copy of a line this device sent (its own plaintext), the
// peer-head table, the group's record, and the mark last.
//
// AN ERASE THAT STOPS PART WAY IS FINISHED, NOT LOST (msgrepo ledger §7, 2026-10-03, review H1). A
// file another process holds open, or a crash, can stop it after the epoch states are gone, and
// without the mark that group could be neither restored nor named again. With it, ForgetGroup
// answers [ErrForgetUnfinished] -- the device HAS left -- and calling it again, or the next
// [Device.Restore], finishes the erase. A group being left is never restored.
//
// WHAT IT DOES NOT DO, and a caller that says otherwise to a person is overclaiming:
//   - it tells nobody. There is no content kind for "left", so the others go on seeing this device
//     as a member, and go on sealing to its leaf, until somebody removes it;
//   - it does not remove the leaf, so until somebody does, this identity cannot be added back (R6a
//     refuses to add an identity while any leaf of the tree carries it). An admin is removed only by
//     the owner, and an owner's leaf by nobody;
//   - it does not touch the server's ciphertext or anybody else's copy, and there is no Unsubscribe,
//     so the server may go on announcing the group to this connection;
//   - it keeps the stream-index reservations, which are not secret, because a reserver that never
//     rewinds is the one that never reuses a nonce if this device is ever added back.
//
// AN OWNER IS REFUSED, by [ErrOwnerMustTransfer], when this device is its identity's last leaf and
// somebody else is in the group: nobody removes an owner's leaf (ruling 11), so leaving would strand
// every other member with an owner who is gone for good, and could never be removed or added back.
// MASTER §11: "the leave action is refused for an OWNER until ownership has been transferred". An
// owner alone, or one with another device of its own still in the group, strands nobody and may go.
// A refusal changes nothing: no mark, no close, no file.
//
// ONE CALLER AT A TIME PER GROUP. A [Device.Restore] or [Device.Join] of the same group on another
// goroutine, between the map drop and the erase, is not guarded against: Restore skips a marked
// group, but a Join would build a group the erase then deletes under it. Every caller this package
// has drives a device from one worker.
func (self *Device) ForgetGroup(groupId []byte) error {
	store, durable := self.stateStore.(DeviceStore)
	self.mutex.Lock()
	group, held := self.groups[string(groupId)]
	self.mutex.Unlock()

	// A LEAVE ALREADY UNDER WAY is finished whatever state it stopped in: closed or not, held or
	// not. The owner question was asked when the mark went down, and is not asked again of a group
	// whose keys may already be gone.
	if durable {
		marked, err := store.GroupBeingForgotten(groupId)
		if err != nil {
			return err
		}
		if marked {
			if held {
				group.Close()
				self.drop(groupId, group)
			}
			if err := store.DeleteGroupRecord(groupId); err != nil {
				return fmt.Errorf("%w: %w", ErrForgetUnfinished, err)
			}
			return nil
		}
	}
	if !held {
		return fmt.Errorf("%w: %x", ErrGroupNotHeld, groupId)
	}

	mark := func() error {
		if !durable {
			return nil
		}
		return store.MarkGroupForgetting(groupId)
	}
	stands := func() bool {
		if !durable {
			return false
		}
		marked, err := store.GroupBeingForgotten(groupId)
		return err == nil && marked
	}
	closed, err := group.closeToForget(mark, stands)
	if !closed {
		return err
	}
	// FROM HERE THIS DEVICE HAS LEFT. A close that reported an error still closed the group and
	// still erased its secrets in memory ([Group.closeLocked] is unconditional about both), so the
	// leave goes on, and that error is not the leave's: the device did leave. On a store that is not
	// durable there is nothing on the disk to erase, and leaving is the close and the drop.
	self.drop(groupId, group)
	if !durable {
		return nil
	}
	if err := store.DeleteGroupRecord(groupId); err != nil {
		return fmt.Errorf("%w: %w", ErrForgetUnfinished, err)
	}
	return nil
}

// finishLeaveBefore is what [Device.Join] and [Device.CreateGroup] owe a leave of the same group
// id that has not finished erasing (msgrepo ledger §7, 2026-10-03, re-check R1). The mark is the
// erase's own instruction, so state written beside it is erased with it at the next Restore: a
// group re-joined over a standing mark worked until the restart and was then gone, with no
// warning. The erase is finished first, or the join is refused.
func (self *Device) finishLeaveBefore(groupId []byte) error {
	store, durable := self.stateStore.(DeviceStore)
	if !durable {
		return nil
	}
	marked, err := store.GroupBeingForgotten(groupId)
	if err != nil {
		return err
	}
	if !marked {
		return nil
	}
	if err := store.DeleteGroupRecord(groupId); err != nil {
		return fmt.Errorf("%w: group %x cannot be joined or made until the erase of the leave before it has finished: %w",
			ErrForgetUnfinished, groupId, err)
	}
	return nil
}

// drop takes one group out of this device's map, if it is still the one there.
func (self *Device) drop(groupId []byte, group *Group) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.groups[string(groupId)] == group {
		delete(self.groups, string(groupId))
	}
}

// closeToForget is the owner's refusal, the leave mark and [Group.Close], IN ONE CRITICAL SECTION,
// and it answers whether the group is closed now. Released between the refusal and the close, an
// ingest could make this device the owner after the check said it was not -- a transfer to it
// arriving in the Receive a UI runs beside the button -- and the group would be stranded exactly as
// the refusal exists to prevent. A group already closed is not one this call can judge, and is
// refused as not held: a leave that closed it and stopped has left its mark, and ForgetGroup
// finishes that one before it gets here.
func (self *Group) closeToForget(mark func() error, stands func() bool) (bool, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return false, fmt.Errorf("%w: it is already closed", ErrGroupNotHeld)
	}
	if err := self.ownerMayNotLeaveLocked(); err != nil {
		return false, err
	}
	if err := mark(); err != nil {
		// A MARK CAN BE ON THE DISK ALTHOUGH ITS WRITE REPORTED A FAILURE (re-check R2): the store
		// renames, then syncs the directory, and answers the sync's error. A mark that stands is an
		// instruction to erase this group at the next Restore, so refusing here would leave a group
		// that works now and is gone at the next launch. Where it stands, the leave goes on.
		if !stands() {
			return false, fmt.Errorf("urmessage: the leave could not be recorded on the disk, so nothing was left: %w", err)
		}
	}
	return true, self.closeLocked()
}

// ownerMayNotLeaveLocked is the refusal, under the lock. A roster that cannot be read is a refusal
// rather than a pass: it is the one reading that says whether this device owns the group, and
// guessing "no" is the guess that strands. A device a valid commit has REMOVED needs no case of
// its own: an owner is never removed (ruling 11), and its roster still reads at the epoch it was
// removed at, so it answers "not the owner" like any member (TestARemovedDeviceMayLeave).
func (self *Group) ownerMayNotLeaveLocked() error {
	members, err := self.membersLocked()
	if err != nil {
		return fmt.Errorf("urmessage: who owns this group could not be read, so leaving it is refused: %w", err)
	}
	var own *Member
	for at := range members {
		if members[at].Mine {
			own = &members[at]
			break
		}
	}
	if own == nil || own.Role != mls.RoleOwner.String() {
		return nil
	}
	somebodyElse := false
	for _, member := range members {
		if member.Mine {
			continue
		}
		if bytes.Equal(member.IdentityPub, own.IdentityPub) {
			// another device of the owner's own: the identity stays, and so does its ownership
			return nil
		}
		somebodyElse = true
	}
	if somebodyElse {
		return fmt.Errorf("%w: %d other leaf/leaves", ErrOwnerMustTransfer, len(members)-1)
	}
	return nil
}
