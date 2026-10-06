package sdk

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Subprotocols on the device (connect/SUBPROTOCOL.md §8.9): an application
// enables a subprotocol id and receives that id's raw message bytes through a
// listener, managing its own codec in application code; it sends raw bytes
// the same way, and can ask a peer which subprotocols it supports. Everything
// typed and allocation-free lives in connect; this surface is the binding
// boundary, and every byte slice that crosses it is a copy.
//
// The registrations live on the device, not on the connect client, so they
// survive whatever client the device holds: the enabled set is applied to the
// device's own client (the one peers address by the device's client id) when
// the device is built, and re-applied by attachSubprotocolsToClient whenever
// that client is replaced.

// Receives one subprotocol message. Called inline on the client's receive
// goroutine and must not block; messageBytes is the listener's own copy.
type SubprotocolListener interface {
	SubprotocolMessage(subprotocolId int32, sourceClientId *Id, messageBytes []byte)
}

// Receives the answer to QuerySubprotocols: the ids the peer supports, or
// ok=false when the query failed or timed out (an old peer never answers).
type SubprotocolsQueryCallback interface {
	Result(subprotocolIds *IntList, ok bool)
}

// Monotonic counters of the device client's subprotocol traffic.
type SubprotocolStats struct {
	Sent                int64
	SentByteCount       int64
	Received            int64
	ReceivedByteCount   int64
	DroppedUnregistered int64
	DroppedDecode       int64
	MarshalOverrun      int64
	// queries this device sent, queries it answered, and answers it could
	// not enqueue on the companion reply path
	QueriesSent     int64
	QueriesAnswered int64
	QueryReplyDrops int64
}

// Ids below this are the network's (connect.SubprotocolReservedLimit).
const SubprotocolReservedLimit = int32(connect.SubprotocolReservedLimit)

// The device's enabled subprotocols and their listeners, keyed by id, plus
// the connect raw callback registered per id on the attached client.
type deviceLocalSubprotocols struct {
	ctx context.Context
	log connect.Logger

	stateLock sync.Mutex
	client    *connect.Client
	// per id, the listeners and the removal of the client raw callback
	entries map[int32]*deviceLocalSubprotocolEntry
}

type deviceLocalSubprotocolEntry struct {
	listeners *connect.CallbackList[SubprotocolListener]
	// nil when no client is attached
	removeRawCallback func()
}

func newDeviceLocalSubprotocols(ctx context.Context, log connect.Logger) *deviceLocalSubprotocols {
	return &deviceLocalSubprotocols{
		ctx:     ctx,
		log:     log,
		entries: map[int32]*deviceLocalSubprotocolEntry{},
	}
}

func checkDeviceSubprotocolId(subprotocolId int32) error {
	if subprotocolId <= 0 || 0xFFFF < subprotocolId {
		return fmt.Errorf("subprotocol id %d out of range 1..65535", subprotocolId)
	}
	if subprotocolId < SubprotocolReservedLimit {
		return fmt.Errorf("subprotocol id %d is reserved for the network (below %d)", subprotocolId, SubprotocolReservedLimit)
	}
	return nil
}

// Applies the enabled set to client, dropping the registrations on the
// previous client. A nil client detaches.
func (self *deviceLocalSubprotocols) attach(client *connect.Client) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.client = client
	for subprotocolId, entry := range self.entries {
		if entry.removeRawCallback != nil {
			entry.removeRawCallback()
			entry.removeRawCallback = nil
		}
		if client != nil {
			self.registerWithLock(subprotocolId, entry)
		}
	}
}

// Registers the raw callback for one id on the attached client; the
// callback fans the message out to the id's listeners.
func (self *deviceLocalSubprotocols) registerWithLock(subprotocolId int32, entry *deviceLocalSubprotocolEntry) {
	if self.client == nil || entry.removeRawCallback != nil {
		return
	}
	listeners := entry.listeners
	remove, err := self.client.AddSubprotocolRawCallback(
		connect.SubprotocolId(subprotocolId),
		func(source connect.TransferPath, id connect.SubprotocolId, messageBytes []byte, peer connect.Peer) {
			self.deliver(listeners, subprotocolId, source, messageBytes)
		},
	)
	if err != nil {
		self.log.Errorf("[subprotocol]could not register %d: %s\n", subprotocolId, err)
		return
	}
	entry.removeRawCallback = remove
}

// Hands one message to the listeners. The bytes the client delivers are a
// borrowed view of the frame; the listeners get one pooled copy that is
// released after they return, so a listener that keeps the bytes must copy.
func (self *deviceLocalSubprotocols) deliver(listeners *connect.CallbackList[SubprotocolListener], subprotocolId int32, source connect.TransferPath, messageBytes []byte) {
	callbacks := listeners.Get()
	if len(callbacks) == 0 {
		return
	}
	retained, release := connect.RetainSubprotocolBytes(messageBytes)
	defer release()
	sourceClientId := newId([16]byte(source.SourceId))
	for _, listener := range callbacks {
		connect.HandleError(func() {
			listener.SubprotocolMessage(subprotocolId, sourceClientId, retained)
		})
	}
}

// Enables subprotocolId for listener. Any number of listeners may share an
// id; the Sub removes this one, and the id's client registration goes with
// the last listener. Reserved and out-of-range ids are refused.
func (self *deviceLocalSubprotocols) enable(subprotocolId int32, listener SubprotocolListener) (Sub, error) {
	if listener == nil {
		return nil, errors.New("nil listener")
	}
	if err := checkDeviceSubprotocolId(subprotocolId); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry := self.entries[subprotocolId]
	if entry == nil {
		entry = &deviceLocalSubprotocolEntry{
			listeners: connect.NewCallbackList[SubprotocolListener](),
		}
		self.entries[subprotocolId] = entry
	}
	callbackId := entry.listeners.Add(listener)
	self.registerWithLock(subprotocolId, entry)
	return newSub(func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		current := self.entries[subprotocolId]
		if current != entry {
			return
		}
		entry.listeners.Remove(callbackId)
		if len(entry.listeners.Get()) == 0 {
			self.disableWithLock(subprotocolId)
		}
	}), nil
}

// Removes every listener of subprotocolId and its client registration.
func (self *deviceLocalSubprotocols) disable(subprotocolId int32) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.disableWithLock(subprotocolId)
}

func (self *deviceLocalSubprotocols) disableWithLock(subprotocolId int32) {
	entry := self.entries[subprotocolId]
	if entry == nil {
		return
	}
	if entry.removeRawCallback != nil {
		entry.removeRawCallback()
		entry.removeRawCallback = nil
	}
	delete(self.entries, subprotocolId)
}

// The enabled ids, sorted.
func (self *deviceLocalSubprotocols) enabledIds() []int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	ids := make([]int32, 0, len(self.entries))
	for subprotocolId := range self.entries {
		ids = append(ids, subprotocolId)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (self *deviceLocalSubprotocols) attachedClient() *connect.Client {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.client
}

// Sends the caller's bytes as one message of subprotocolId. The bytes are
// copied once into the frame (the same cost as a protobuf marshal), so the
// caller keeps its slice. False when there is no client, the id is invalid,
// the destination is the control id, or the send could not be enqueued.
func (self *deviceLocalSubprotocols) sendBytes(subprotocolId int32, destinationClientId *Id, messageBytes []byte) bool {
	if destinationClientId == nil {
		return false
	}
	if subprotocolId <= 0 || 0xFFFF < subprotocolId {
		return false
	}
	client := self.attachedClient()
	if client == nil {
		return false
	}
	// the client takes ownership of the slice it is given; hand it a copy so
	// the caller's (or the binding's) buffer stays the caller's
	return client.SendSubprotocolBytes(
		connect.SubprotocolId(subprotocolId),
		slices.Clone(messageBytes),
		destinationClientId.toConnectId(),
		func(err error) {},
	)
}

// Asks the peer which subprotocols it supports and answers on callback from a
// worker; the caller never blocks. A timeout of zero or less means the
// device's default.
func (self *deviceLocalSubprotocols) query(destinationClientId *Id, timeoutMillis int64, callback SubprotocolsQueryCallback) {
	if callback == nil {
		return
	}
	client := self.attachedClient()
	if client == nil || destinationClientId == nil {
		connect.HandleError(func() {
			callback.Result(NewIntList(), false)
		})
		return
	}
	timeout := time.Duration(timeoutMillis) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultSubprotocolsQueryTimeout
	}
	destinationId := destinationClientId.toConnectId()
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(self.ctx, timeout)
		defer cancel()
		ids, err := client.QuerySubprotocols(ctx, destinationId)
		list := NewIntList()
		if err != nil {
			self.log.Infof("[subprotocol]query %s failed: %s\n", destinationId, err)
			callback.Result(list, false)
			return
		}
		for _, id := range ids {
			list.Add(int(id))
		}
		callback.Result(list, true)
	})
}

const defaultSubprotocolsQueryTimeout = 10 * time.Second

func (self *deviceLocalSubprotocols) stats() *SubprotocolStats {
	client := self.attachedClient()
	if client == nil {
		return &SubprotocolStats{}
	}
	snapshot := client.SubprotocolStats()
	return &SubprotocolStats{
		Sent:                int64(snapshot.Sent),
		SentByteCount:       int64(snapshot.SentByteCount),
		Received:            int64(snapshot.Received),
		ReceivedByteCount:   int64(snapshot.ReceivedByteCount),
		DroppedUnregistered: int64(snapshot.DroppedUnregistered),
		DroppedDecode:       int64(snapshot.DroppedDecode),
		MarshalOverrun:      int64(snapshot.MarshalOverrun),
		QueriesSent:         int64(snapshot.QueriesSent),
		QueriesAnswered:     int64(snapshot.QueriesAnswered),
		QueryReplyDrops:     int64(snapshot.QueryReplyDrops),
	}
}

// Messages delivered to the listeners of one id on this device's client, 0
// when the id is not enabled or no client is attached (the per-id map of the
// client snapshot, one entry at a time, since a map cannot cross the binding).
func (self *deviceLocalSubprotocols) receivedCount(subprotocolId int32) int64 {
	client := self.attachedClient()
	if client == nil {
		return 0
	}
	return int64(client.SubprotocolStats().ReceivedById[connect.SubprotocolId(subprotocolId)])
}

// --- DeviceLocal surface ---

// Enables a subprotocol for listener on this device's client. Ids below
// SubprotocolReservedLimit belong to the network and are refused.
func (self *DeviceLocal) EnableSubprotocol(subprotocolId int32, listener SubprotocolListener) (Sub, error) {
	return self.subprotocols.enable(subprotocolId, listener)
}

// Removes every listener of the id.
func (self *DeviceLocal) DisableSubprotocol(subprotocolId int32) {
	self.subprotocols.disable(subprotocolId)
}

// The enabled subprotocol ids, sorted.
func (self *DeviceLocal) EnabledSubprotocols() *IntList {
	list := NewIntList()
	for _, id := range self.subprotocols.enabledIds() {
		list.Add(int(id))
	}
	return list
}

// Sends one raw subprotocol message to a peer client; fire and forget.
func (self *DeviceLocal) SendSubprotocolBytes(subprotocolId int32, destinationClientId *Id, messageBytes []byte) bool {
	return self.subprotocols.sendBytes(subprotocolId, destinationClientId, messageBytes)
}

// Asks a peer which subprotocols it supports; the answer arrives on callback
// from a worker.
func (self *DeviceLocal) QuerySubprotocols(destinationClientId *Id, timeoutMillis int64, callback SubprotocolsQueryCallback) {
	self.subprotocols.query(destinationClientId, timeoutMillis, callback)
}

// Messages delivered to the listeners of one enabled id.
func (self *DeviceLocal) SubprotocolReceivedCount(subprotocolId int32) int64 {
	return self.subprotocols.receivedCount(subprotocolId)
}

func (self *DeviceLocal) SubprotocolStats() *SubprotocolStats {
	return self.subprotocols.stats()
}

// Applies the device's enabled subprotocols to client (nil detaches). Called
// when the device's client is built or replaced.
func (self *DeviceLocal) attachSubprotocolsToClient(client *connect.Client) {
	self.subprotocols.attach(client)
}
