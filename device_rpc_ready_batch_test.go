package sdk

import (
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
)

type deviceRpcReadyBatchTestWriter struct {
	*boundedDeviceRpcTestWs
	write       func([][]byte) error
	deadlineErr error
}

func (w *deviceRpcReadyBatchTestWriter) WriteMessages(messages [][]byte) error {
	if w.write != nil {
		return w.write(messages)
	}
	return nil
}

func (w *deviceRpcReadyBatchTestWriter) SetWriteDeadline(time.Time) error { return w.deadlineErr }

func newDeviceRpcReadyBatchTestMux(t *testing.T, writer *deviceRpcReadyBatchTestWriter) *deviceRpcMux {
	s := defaultDeviceRpcSettings()
	s.MuxSendBufferSize = 64 // enough to prove the unchanged 32-message drain bound
	s.DisableLogging = true
	return newDeviceRpcSendDrainTestMux(t, writer, s)
}

func assertDeviceRpcReadyDescriptorsCleared(t *testing.T, mux *deviceRpcMux) {
	t.Helper()
	for i, message := range mux.writeMessages {
		if message != nil {
			t.Errorf("writer descriptor %d retained a returned payload", i)
		}
	}
}

func TestDeviceRpcReadyBatchReusableDescriptorsAndOrder(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		frameBytes, count, take int
	}{
		{"message-limit", 64, 35, 32},
		{"byte-limit", 1200, 20, 11},
		{"ready-only-singleton", 256, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &deviceRpcReadyBatchTestWriter{boundedDeviceRpcTestWs: newBoundedDeviceRpcTestWs(nil)}
			mux := newDeviceRpcReadyBatchTestMux(t, writer)
			// Reuse the same writer-owned descriptors in successive flushes.
			for round := range 2 {
				var observed [][]byte
				for i := range tc.count {
					payload := make([]byte, tc.frameBytes-1)
					payload[0] = byte(i)
					if _, err := mux.conns[i%2].write(payload, func(frame []byte) {
						observed = append(observed, connect.MessagePoolShareReadOnly(frame))
					}); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				writer.write = func(messages [][]byte) error {
					calls++
					if len(messages) != tc.take || &messages[0] != &mux.writeMessages[0] {
						t.Fatalf("flush did not borrow bounded writer storage: %d messages", len(messages))
					}
					for i, message := range messages {
						if len(message) != tc.frameBytes || message[0] != byte(i%2) || message[1] != byte(i) {
							t.Errorf("round %d position %d reordered forward/reverse traffic", round, i)
						}
					}
					return nil
				}
				if err := mux.writeReadyMessages(writer, <-mux.send); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || len(mux.send) != tc.count-tc.take || mux.sendBytes.used != int64((tc.count-tc.take)*tc.frameBytes) {
					t.Fatal("ready flush changed the message/byte bound or waited for future traffic")
				}
				assertDeviceRpcReadyDescriptorsCleared(t, mux)
				for len(mux.send) != 0 {
					message := <-mux.send
					connect.MessagePoolReturn(message)
					mux.sendBytes.release(len(message))
				}
				for _, frame := range observed {
					assertDeviceRpcObservedFrameReturned(t, frame)
				}
			}
			mux.finishSend()
			assertDeviceRpcSendDrained(t, mux)
		})
	}
}

func TestDeviceRpcReadyBatchDescriptorsClearedOnFailure(t *testing.T) {
	for _, mode := range []string{"canceled", "write-error", "deadline-error"} {
		t.Run(mode, func(t *testing.T) {
			writer := &deviceRpcReadyBatchTestWriter{boundedDeviceRpcTestWs: newBoundedDeviceRpcTestWs(nil)}
			mux := newDeviceRpcReadyBatchTestMux(t, writer)
			var observed [][]byte
			for i := range 3 {
				if _, err := mux.conns[i%2].write([]byte("payload"), func(frame []byte) {
					observed = append(observed, connect.MessagePoolShareReadOnly(frame))
				}); err != nil {
					t.Fatal(err)
				}
			}
			first := <-mux.send
			sentinel := errors.New("terminal test failure")
			calls := 0
			writer.write = func([][]byte) error { calls++; return sentinel }
			switch mode {
			case "canceled":
				mux.cancel()
				sentinel = io.ErrClosedPipe
			case "deadline-error":
				mux.writeTimeout = time.Second
				writer.deadlineErr = sentinel
			}
			if err := mux.writeReadyMessages(writer, first); !errors.Is(err, sentinel) {
				t.Fatalf("wrong terminal result: %v", err)
			}
			if mode != "write-error" && calls != 0 {
				t.Fatal("failed admission/deadline wrote payload bytes")
			}
			assertDeviceRpcReadyDescriptorsCleared(t, mux)
			mux.finishSend()
			assertDeviceRpcSendDrained(t, mux)
			for _, frame := range observed {
				assertDeviceRpcObservedFrameReturned(t, frame)
			}
		})
	}
}

func TestDeviceRpcReadyBatchBlockedWriteCancellation(t *testing.T) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		writer := &deviceRpcReadyBatchTestWriter{boundedDeviceRpcTestWs: newBoundedDeviceRpcTestWs(nil)}
		mux := newDeviceRpcReadyBatchTestMux(t, writer)
		started := make(chan struct{})
		writer.write = func(messages [][]byte) error {
			if len(messages) != 1 {
				t.Error("unexpected blocked flush")
			}
			close(started)
			<-mux.ctx.Done()
			return io.ErrClosedPipe
		}
		var observed [][]byte
		enqueue := func(tag uint8) {
			t.Helper()
			if _, err := mux.conns[tag].write([]byte("payload"), func(frame []byte) {
				observed = append(observed, connect.MessagePoolShareReadOnly(frame))
			}); err != nil {
				t.Fatal(err)
			}
		}
		enqueue(0)
		finished := startDeviceRpcSendDrainTestWriter(mux)
		<-started
		enqueue(1)
		enqueue(0)
		mux.close()
		synctest.Wait()
		<-finished
		assertDeviceRpcReadyDescriptorsCleared(t, mux)
		assertDeviceRpcSendDrained(t, mux)
		for _, frame := range observed {
			assertDeviceRpcObservedFrameReturned(t, frame)
		}
	})
}

func TestDeviceRpcReadyBatchNoPerFlushDescriptorAllocation(t *testing.T) {
	for _, count := range []int{1, 11, 32} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			writer := &deviceRpcReadyBatchTestWriter{boundedDeviceRpcTestWs: newBoundedDeviceRpcTestWs(nil)}
			mux := newDeviceRpcReadyBatchTestMux(t, writer)
			defer mux.finishSend()
			allocations := testing.AllocsPerRun(1000, func() {
				for range count {
					message := connect.MessagePoolGet(256)
					if !mux.sendBytes.tryAcquire(len(message)) {
						panic("unexpected test admission refusal")
					}
					mux.send <- message
				}
				if err := mux.writeReadyMessages(writer, <-mux.send); err != nil {
					panic(err)
				}
			})
			if allocations != 0 {
				t.Fatalf("ready flush allocated %.3f times; writer descriptors must be reusable", allocations)
			}
			assertDeviceRpcReadyDescriptorsCleared(t, mux)
			if mux.sendBytes.used != 0 || len(mux.send) != 0 {
				t.Fatal("allocation control retained ownership")
			}
		})
	}
}
