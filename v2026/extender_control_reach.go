package sdk

import (
	"context"
	"sync"

	"github.com/urnetwork/connect/v2026"
)

// extender_control_reach.go — how the sdk regains reach to the control route
// when ordinary discovery has failed (connect/EXTENDER-UPDATE2.md §1.1 rule 4,
// §2.3).
//
// The gated release, and every authenticated read after it, travels the
// control route through the space's client strategy. Where the direct path is
// blocked and only extenders reach the operator, the route that failed must
// not be the only way back to it. When a release fetch cannot reach the
// operator at all, it asks each reach of the space in turn to make verified
// candidates dialable that do not depend on the failed route, and retries at
// once, within its own retry budget, when one did.
//
// The order is fixed: the space's own cached verified open records first
// (extenderCachedOpenReach), which costs no dial, then every reach added with
// addExtenderControlReach, in the order added -- the authenticated seed
// bootstrap of §2 is one. A reach never makes an unsigned hint dialable and
// never changes a trust anchor: whatever it makes available is a record the
// directory verified under the root keys in force.
//
// The list is ready from the space's construction on, whichever of the
// space's parts starts first: a reach added before the release fetch starts
// is kept.

// One way to regain control reach.
type extenderControlReach interface {
	// Makes verified extender candidates available that can carry the control
	// route without the route that failed. Returns true only when a verified
	// open record became usable, so the caller retries at once, and false when
	// it had nothing to add. It returns once ctx is done at the latest. It may
	// be called from more than one goroutine; an implementation that dials
	// coalesces its own passes.
	obtainExtenderControlReach(ctx context.Context) bool
}

// The reaches one space adds after its own cached open records. The zero
// value is an empty list.
type extenderControlReachList struct {
	stateLock sync.Mutex
	nextId    uint64
	reaches   map[uint64]extenderControlReach
	order     []uint64
}

// Adds one reach after the ones already added, and returns its removal, which
// is idempotent.
func (self *extenderControlReachList) add(reach extenderControlReach) (remove func()) {
	if reach == nil {
		return func() {}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.reaches == nil {
		self.reaches = map[uint64]extenderControlReach{}
	}
	id := self.nextId
	self.nextId += 1
	self.reaches[id] = reach
	self.order = append(self.order, id)
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if _, ok := self.reaches[id]; !ok {
			return
		}
		delete(self.reaches, id)
		for i, orderId := range self.order {
			if orderId == id {
				self.order = append(self.order[:i:i], self.order[i+1:]...)
				break
			}
		}
	}
}

// The reaches in the order added, read with no lock held after.
func (self *extenderControlReachList) list() []extenderControlReach {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reaches := make([]extenderControlReach, 0, len(self.order))
	for _, id := range self.order {
		reaches = append(reaches, self.reaches[id])
	}
	return reaches
}

// The reach of the space's own directory: the verified open records it has
// cached. It dials nothing. When the gated preference is holding them back
// for a gated attempt, it lets them carry the control route now (connect
// ExtenderDirectory.AllowOpenForControlReach) rather than at the end of the
// attempt's budget.
type extenderCachedOpenReach struct {
	directory *connect.ExtenderDirectory
}

func (self *extenderCachedOpenReach) obtainExtenderControlReach(ctx context.Context) bool {
	if self.directory == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	default:
	}
	return self.directory.AllowOpenForControlReach()
}

// Adds a reach the release fetch tries after the space's own cached open
// records, such as the authenticated seed bootstrap (§2.3), and returns its
// removal. A space with no directory has nothing to reach with, and keeps
// nothing.
func (self *NetworkSpace) addExtenderControlReach(reach extenderControlReach) (remove func()) {
	if self == nil || self.extenderDirectory == nil {
		return func() {}
	}
	return self.extenderRelease.reaches.add(reach)
}

// Every reach of the space in the order they are tried: the cached open
// records, then the added reaches.
func (self *NetworkSpace) extenderControlReachOrder() []extenderControlReach {
	if self.extenderDirectory == nil {
		return []extenderControlReach{}
	}
	reaches := []extenderControlReach{
		&extenderCachedOpenReach{directory: self.extenderDirectory},
	}
	return append(reaches, self.extenderRelease.reaches.list()...)
}

// Asks the control route: `/hello` through the space's client strategy, which
// takes whatever route the strategy has -- direct while the directory is empty,
// a verified extender once one is usable. Nil is an answer from the operator.
func (self *NetworkSpace) askExtenderControl(ctx context.Context) error {
	request, err := connect.HelloRequestFromUrl(ctx, self.apiUrl, "")
	if err != nil {
		return err
	}
	_, err = connect.HttpGetWithStrategyRaw(ctx, self.clientStrategy, request.URL.String(), "")
	return err
}

// Tries each reach of the space in order and stops at the first that made
// something new dialable. Reports whether one did.
func (self *NetworkSpace) obtainExtenderControlReach(ctx context.Context) bool {
	for _, reach := range self.extenderControlReachOrder() {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if reach.obtainExtenderControlReach(ctx) {
			return true
		}
	}
	return false
}
