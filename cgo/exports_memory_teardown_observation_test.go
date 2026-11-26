// A device's memory teardown observation through the real c abi (QD-1423), as a c host runs it: it begins the
// observation on its device and holds the handle the constructor answers, reads the observer's id, closes the device,
// waits for the device's join, finishes the observation and takes its json receipt, and releases the handle. An
// observation aborted, or begun on a device already closed, takes its failed receipt and never reads as a close, and a
// handle that is no live device, or no live observation, answers nothing. Each device is built as the extender tests
// build theirs, on a space that dials nothing.
package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/urnetwork/sdk"
)

// How long a test lets an observation wait for a device or for its own goroutine, in milliseconds. The observation
// caps every wait at its own lifetime.
const testingMemoryTeardownTimeoutMilliseconds = int64(60 * 1000)

// The observation's json receipt, the fields these tests read.
type testingMemoryTeardownReceipt struct {
	Type             string `json:"type"`
	SchemaVersion    int    `json:"schemaVersion"`
	ObserverId       string `json:"observerId"`
	State            string `json:"state"`
	Failure          string `json:"failure"`
	CancelSequence   int64  `json:"cancelSequence"`
	JoinSequence     int64  `json:"joinSequence"`
	TerminalSequence int64  `json:"terminalSequence"`
	ObserverJoined   bool   `json:"observerJoined"`
	Samples          []struct {
		Sequence int64  `json:"sequence"`
		Stage    string `json:"stage"`
	} `json:"samples"`
}

// A space whose devices dial nothing, as a handle.
func testingMemoryTeardownSpace(t *testing.T) uint64 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	networkSpace := sdk.NewNetworkSpaceWithUrls(ctx, "https://192.0.2.1", "wss://192.0.2.1", testingOfflineClientStrategySettings())
	t.Cleanup(func() {
		networkSpace.Close()
	})
	// the devices never reach an api: the test is the observation
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	spaceHandle := newHandle(networkSpace)
	t.Cleanup(func() {
		handleRelease(spaceHandle)
	})
	return spaceHandle
}

// A device built through urnet_new_device_local with the default settings. The cleanup closes it, whether or not the
// test closed it through the abi, waits for its join and releases its handle.
func testingMemoryTeardownDevice(t *testing.T, spaceHandle uint64) uint64 {
	t.Helper()
	byJwt := cString(testingProvideExtenderClientJwt())
	defer cStringFree(byJwt)
	empty := cString("")
	defer cStringFree(empty)
	instanceId := cString(sdk.NewId().String())
	defer cStringFree(instanceId)
	outError := socketABIOut(urnet_new_device_local, 7)
	deviceHandle := socketABICall(
		urnet_new_device_local,
		spaceHandle,
		byJwt,
		empty,
		empty,
		empty,
		instanceId,
		nil,
		outError,
	)[0].Uint()
	if err := socketABIError(outError); err != "" || deviceHandle == 0 {
		t.Fatalf("the device was not built: %q", err)
	}
	value, _ := handleValue(deviceHandle)
	device, ok := value.(*sdk.DeviceLocal)
	if !ok {
		t.Fatalf("the device handle holds %T", value)
	}
	t.Cleanup(func() {
		device.Close()
		if !device.WaitForClose(testingMemoryTeardownTimeoutMilliseconds) {
			t.Error("the device did not join")
		}
		handleRelease(deviceHandle)
	})
	return deviceHandle
}

// Begins an observation through the abi: its handle, and the error text the export set.
func testingBeginMemoryTeardownObservation(deviceHandle uint64) (uint64, string) {
	outError := socketABIOut(urnet_device_local_begin_memory_teardown_observation, 1)
	observationHandle := socketABICall(urnet_device_local_begin_memory_teardown_observation, deviceHandle, outError)[0].Uint()
	return observationHandle, socketABIError(outError)
}

// The text of a char* an export answered, which the library then frees, and whether it answered one at all.
func testingTakeAbiString(result reflect.Value) (string, bool) {
	if result.IsNil() {
		return "", false
	}
	text := goStringAt(result.UnsafePointer())
	socketABICall(urnet_free_string, result)
	return text, true
}

// Finishes the observation, or aborts it, through the abi: the receipt's json, decoded.
func testingTakeMemoryTeardownReceipt(t *testing.T, takeJson any, observationHandle uint64) (string, testingMemoryTeardownReceipt) {
	t.Helper()
	receiptJson, ok := testingTakeAbiString(socketABICall(takeJson, observationHandle, testingMemoryTeardownTimeoutMilliseconds)[0])
	if !ok {
		t.Fatal("the observation answered no receipt")
	}
	var receipt testingMemoryTeardownReceipt
	if err := json.Unmarshal([]byte(receiptJson), &receipt); err != nil {
		t.Fatalf("the receipt does not decode: %v: %s", err, receiptJson)
	}
	if receipt.Type != "device-memory-teardown" || receipt.SchemaVersion != 1 || !receipt.ObserverJoined || len(receipt.Samples) == 0 ||
		receipt.Samples[0].Stage != "begin" {
		t.Fatalf("the receipt is no joined observation's: %s", receiptJson)
	}
	return receiptJson, receipt
}

// Whether the observation reads the device as closed, through the abi.
func testingMemoryTeardownWait(observationHandle uint64) bool {
	return socketABICall(urnet_memory_teardown_observation_wait_for_device_close, observationHandle, testingMemoryTeardownTimeoutMilliseconds)[0].Bool()
}

// Construct, the four methods and release: the observation sees the device's cancel and join in order and completes,
// its receipt is the same however it is taken again, and once released its handle answers nothing.
func TestMemoryTeardownObservationLifecycleThroughTheAbi(t *testing.T) {
	deviceHandle := testingMemoryTeardownDevice(t, testingMemoryTeardownSpace(t))

	liveHandleCount := socketABICall(urnet_live_handle_count)[0].Int()
	observationHandle, err := testingBeginMemoryTeardownObservation(deviceHandle)
	if err != "" || observationHandle == 0 {
		t.Fatalf("the observation did not begin: %q", err)
	}
	if count := socketABICall(urnet_live_handle_count)[0].Int(); count != liveHandleCount+1 {
		t.Fatalf("%d live handles after the observation began, %d before", count, liveHandleCount)
	}

	observerId, ok := testingTakeAbiString(socketABICall(urnet_memory_teardown_observation_get_observer_id, observationHandle)[0])
	if !ok {
		t.Fatal("the observation answered no observer id")
	}
	if _, err := sdk.ParseId(observerId); err != nil {
		t.Fatalf("the observer id %q is no id: %v", observerId, err)
	}
	if again, _ := testingTakeAbiString(socketABICall(urnet_memory_teardown_observation_get_observer_id, observationHandle)[0]); again != observerId {
		t.Fatalf("the observer id changed from %q to %q", observerId, again)
	}

	socketABICall(urnet_device_close, deviceHandle)
	if !testingMemoryTeardownWait(observationHandle) {
		t.Fatal("the observation did not see the device join")
	}
	finishedJson, finished := testingTakeMemoryTeardownReceipt(t, urnet_memory_teardown_observation_finish_and_take_json, observationHandle)
	if finished.ObserverId != observerId || finished.State != "complete" || finished.Failure != "" {
		t.Fatalf("the finished receipt is %s", finishedJson)
	}
	if !(0 < finished.CancelSequence && finished.CancelSequence < finished.JoinSequence && finished.JoinSequence < finished.TerminalSequence) {
		t.Fatalf("the receipt's cancel, join and terminal are out of order: %s", finishedJson)
	}
	stageSequences := map[string]int64{}
	for _, sample := range finished.Samples {
		stageSequences[sample.Stage] = sample.Sequence
	}
	if stageSequences["cancelled"] != finished.CancelSequence || stageSequences["joined"] != finished.JoinSequence ||
		finished.Samples[len(finished.Samples)-1].Stage != "terminal" {
		t.Fatalf("the receipt's samples do not hold its sequences: %s", finishedJson)
	}

	// the receipt is taken once: an abort after the finish answers the same one, and the observation still reads as
	// a completed close
	if abortedJson, _ := testingTakeMemoryTeardownReceipt(t, urnet_memory_teardown_observation_abort_and_take_json, observationHandle); abortedJson != finishedJson {
		t.Errorf("an abort after the finish answered %s, the finish %s", abortedJson, finishedJson)
	}
	if !testingMemoryTeardownWait(observationHandle) {
		t.Error("the completed observation does not read as the device's close")
	}

	if !socketABICall(urnet_release, observationHandle)[0].Bool() {
		t.Fatal("the release did not know the observation's handle")
	}
	if socketABICall(urnet_release, observationHandle)[0].Bool() {
		t.Error("a second release knew the handle")
	}
	if count := socketABICall(urnet_live_handle_count)[0].Int(); count != liveHandleCount {
		t.Errorf("%d live handles after the release, %d before the observation", count, liveHandleCount)
	}
	if _, ok := testingTakeAbiString(socketABICall(urnet_memory_teardown_observation_get_observer_id, observationHandle)[0]); ok {
		t.Error("a released handle answered an observer id")
	}
	if testingMemoryTeardownWait(observationHandle) {
		t.Error("a released handle reads as the device's close")
	}
	for _, takeJson := range []any{urnet_memory_teardown_observation_finish_and_take_json, urnet_memory_teardown_observation_abort_and_take_json} {
		if receiptJson, ok := testingTakeAbiString(socketABICall(takeJson, observationHandle, testingMemoryTeardownTimeoutMilliseconds)[0]); ok {
			t.Errorf("a released handle answered a receipt: %s", receiptJson)
		}
	}
}

// An observation aborted while its device runs takes the failed receipt, which keeps its evidence and names the abort,
// and it never reads as the device's close, not even once the device has closed; a finish after it answers the same.
func TestMemoryTeardownObservationAbortedThroughTheAbi(t *testing.T) {
	deviceHandle := testingMemoryTeardownDevice(t, testingMemoryTeardownSpace(t))
	observationHandle, err := testingBeginMemoryTeardownObservation(deviceHandle)
	if err != "" || observationHandle == 0 {
		t.Fatalf("the observation did not begin: %q", err)
	}
	abortedJson, aborted := testingTakeMemoryTeardownReceipt(t, urnet_memory_teardown_observation_abort_and_take_json, observationHandle)
	if aborted.State != "failed" || aborted.Failure != "observer-aborted-or-expired" || aborted.CancelSequence != 0 ||
		aborted.JoinSequence != 0 || aborted.Samples[len(aborted.Samples)-1].Stage != "aborted" {
		t.Fatalf("the aborted receipt is %s", abortedJson)
	}
	if testingMemoryTeardownWait(observationHandle) {
		t.Error("the aborted observation reads as the device's close")
	}
	socketABICall(urnet_device_close, deviceHandle)
	if testingMemoryTeardownWait(observationHandle) {
		t.Error("the aborted observation reads as the device's close once the device closed")
	}
	if finishedJson, _ := testingTakeMemoryTeardownReceipt(t, urnet_memory_teardown_observation_finish_and_take_json, observationHandle); finishedJson != abortedJson {
		t.Errorf("a finish after the abort answered %s, the abort %s", finishedJson, abortedJson)
	}
	if !socketABICall(urnet_release, observationHandle)[0].Bool() {
		t.Error("the release did not know the observation's handle")
	}
}

// Begun on a device already closed, the observation is a handle whose receipt names the late start: it cannot show a
// boundary before the cancel, so it never completes.
func TestMemoryTeardownObservationBegunAfterTheCloseThroughTheAbi(t *testing.T) {
	deviceHandle := testingMemoryTeardownDevice(t, testingMemoryTeardownSpace(t))
	socketABICall(urnet_device_close, deviceHandle)
	observationHandle, err := testingBeginMemoryTeardownObservation(deviceHandle)
	if err != "" || observationHandle == 0 {
		t.Fatalf("the observation did not begin: %q", err)
	}
	receiptJson, receipt := testingTakeMemoryTeardownReceipt(t, urnet_memory_teardown_observation_finish_and_take_json, observationHandle)
	if receipt.State != "failed" || receipt.Failure != "observer-start-after-cancel" {
		t.Fatalf("the late receipt is %s", receiptJson)
	}
	// read once the receipt is taken: before it, a wait may see the device's join first, which it reports as such
	if testingMemoryTeardownWait(observationHandle) {
		t.Error("a late observation reads as the device's close")
	}
	if !socketABICall(urnet_release, observationHandle)[0].Bool() {
		t.Error("the release did not know the observation's handle")
	}
}

// The constructor answers no handle, and sets no error, for a handle that is no live DeviceLocal: the abi's null
// handle, another type's handle and a released device's. The observation's methods answer nothing for the null handle.
func TestMemoryTeardownObservationRefusesAHandleThatIsNoDevice(t *testing.T) {
	spaceHandle := testingMemoryTeardownSpace(t)
	releasedHandle := testingMemoryTeardownDevice(t, spaceHandle)
	socketABICall(urnet_device_close, releasedHandle)
	if !socketABICall(urnet_release, releasedHandle)[0].Bool() {
		t.Fatal("the release did not know the device's handle")
	}
	liveHandleCount := socketABICall(urnet_live_handle_count)[0].Int()
	for _, deviceHandle := range []uint64{0, spaceHandle, releasedHandle} {
		if observationHandle, err := testingBeginMemoryTeardownObservation(deviceHandle); observationHandle != 0 || err != "" {
			t.Errorf("handle %d began observation %d (%q)", deviceHandle, observationHandle, err)
		}
	}
	if count := socketABICall(urnet_live_handle_count)[0].Int(); count != liveHandleCount {
		t.Errorf("%d live handles after the refusals, %d before", count, liveHandleCount)
	}
	if _, ok := testingTakeAbiString(socketABICall(urnet_memory_teardown_observation_get_observer_id, uint64(0))[0]); ok {
		t.Error("the null handle answered an observer id")
	}
	if testingMemoryTeardownWait(0) {
		t.Error("the null handle reads as the device's close")
	}
	for _, takeJson := range []any{urnet_memory_teardown_observation_finish_and_take_json, urnet_memory_teardown_observation_abort_and_take_json} {
		if receiptJson, ok := testingTakeAbiString(socketABICall(takeJson, uint64(0), testingMemoryTeardownTimeoutMilliseconds)[0]); ok {
			t.Errorf("the null handle answered a receipt: %s", receiptJson)
		}
	}
	if socketABICall(urnet_release, uint64(0))[0].Bool() {
		t.Error("the release knew the null handle")
	}
}
