package sdk

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncodedPacketBatchesRespectCountAndByteLimits(t *testing.T) {
	packets := make([][]byte, 140)
	for i := range packets {
		packetSize := 2000
		if i%3 == 0 {
			packetSize = 32
		}
		packets[i] = bytes.Repeat([]byte{byte(i)}, packetSize)
	}

	var decoded [][]byte
	batchCount := 0
	ok := withEncodedPacketBatches(packets, func(batch []byte) {
		batchCount += 1
		if devicePacketBatchMaxByteCount < len(batch) {
			t.Fatalf("batch bytes = %d, limit %d", len(batch), devicePacketBatchMaxByteCount)
		}
		offset := 0
		packetCount := 0
		for offset < len(batch) {
			packetSize := int(binary.BigEndian.Uint16(batch[offset : offset+2]))
			offset += 2
			decoded = append(decoded, bytes.Clone(batch[offset:offset+packetSize]))
			offset += packetSize
			packetCount += 1
		}
		if devicePacketBatchMaxPacketCount < packetCount {
			t.Fatalf("batch packets = %d, limit %d", packetCount, devicePacketBatchMaxPacketCount)
		}
	})
	if !ok {
		t.Fatal("valid packet burst was rejected")
	}
	if batchCount < 2 {
		t.Fatal("large burst was not split")
	}
	if len(decoded) != len(packets) {
		t.Fatalf("decoded %d packets, want %d", len(decoded), len(packets))
	}
	for i := range packets {
		if !bytes.Equal(decoded[i], packets[i]) {
			t.Fatalf("packet %d changed", i)
		}
	}
}

func TestEncodedPacketBatchesRejectInvalidBurstBeforeEmission(t *testing.T) {
	emitted := 0
	ok := withEncodedPacketBatches([][]byte{{1, 2, 3}, nil, {4, 5, 6}}, func([]byte) {
		emitted += 1
	})
	if ok {
		t.Fatal("invalid packet burst was accepted")
	}
	if emitted != 0 {
		t.Fatalf("invalid packet burst emitted %d partial batches", emitted)
	}
}

func TestSendPacketBatchRejectsOversizedFrameBeforeParsing(t *testing.T) {
	device := &DeviceLocal{}
	if accepted := device.SendPacketBatch(make([]byte, devicePacketBatchMaxByteCount+1)); accepted != 0 {
		t.Fatalf("oversized batch accepted %d packets", accepted)
	}
}
