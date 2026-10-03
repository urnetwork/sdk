package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestClientEventConstructors pins the closed builder: every constructor makes
// exactly its name with only its props, negative numbers are "not given", and
// the wire shape carries props.
func TestClientEventConstructors(t *testing.T) {
	e := NewOnboardingStepShownEvent("plan", 2, 1500)
	connect.AssertEqual(t, EventOnboardingStepShown, e.Name)
	connect.AssertEqual(t, true, e.At != "")
	var wire map[string]any
	connect.AssertEqual(t, nil, json.Unmarshal(mustJson(t, e), &wire))
	props := wire["props"].(map[string]any)
	connect.AssertEqual(t, "plan", props["step"])
	connect.AssertEqual(t, 2.0, props["index"])
	connect.AssertEqual(t, 1500.0, props["elapsed_ms"])

	// -1 leaves the optional numbers out
	e = NewOnboardingStepSkippedEvent("widgets", -1, -1)
	connect.AssertEqual(t, `{"step":"widgets"}`, e.PropsJson())

	e = NewOfferScreenShownEvent(OfferSurfaceIntroStep, "offer_screen", "control", PriceTierStandard, 29.99, "USD", 431999)
	connect.AssertEqual(t, EventOfferScreenShown, e.Name)
	connect.AssertEqual(t, nil, json.Unmarshal([]byte(e.PropsJson()), &props))
	connect.AssertEqual(t, "intro_step", props["surface"])
	connect.AssertEqual(t, 29.99, props["price_shown"])
	connect.AssertEqual(t, 431999.0, props["expires_in_s"])

	connect.AssertEqual(t, `{"plan":"yearly"}`, NewOfferCardTappedEvent(PlanYearly).PropsJson())
	connect.AssertEqual(t, `{"plan":"yearly","store":"play"}`, NewOfferCtaTappedEvent(PlanYearly, EventStorePlay).PropsJson())
	connect.AssertEqual(t, `{"control":"back","elapsed_ms":10}`, NewOfferDeclinedEvent(OfferDeclineControlBack, 10).PropsJson())

	e = NewPurchaseFailedEvent(EventStoreStripe, "pro_yearly", PlanYearly, true, 29.99, "USD", "card_declined")
	connect.AssertEqual(t, EventPurchaseFailed, e.Name)
	connect.AssertEqual(t, nil, json.Unmarshal([]byte(e.PropsJson()), &props))
	connect.AssertEqual(t, true, props["trial"])
	connect.AssertEqual(t, "card_declined", props["error_class"])
	connect.AssertEqual(t, `{"store":"apple","trial":false}`, NewPurchaseStartedEvent(EventStoreApple, "", "", false, -1, "").PropsJson())

	connect.AssertEqual(t, "{}", NewConnectFirstEvent().PropsJson())
	connect.AssertEqual(t, `{"kind":"globe"}`, NewWidgetAddedEvent("globe").PropsJson())

	e = NewFeedbackSubmittedEvent(4, "too_slow", "  it works ")
	connect.AssertEqual(t, nil, json.Unmarshal([]byte(e.PropsJson()), &props))
	connect.AssertEqual(t, 4.0, props["rating"])
	connect.AssertEqual(t, true, props["has_text"])
	connect.AssertEqual(t, "it works", props["text"])
	connect.AssertEqual(t, `{"has_text":false}`, NewFeedbackSubmittedEvent(0, "", "").PropsJson())
	connect.AssertEqual(t, `{"product_updates":false}`, NewSignupOptoutChangedEvent(false).PropsJson())

	// every constructor's props are within the schema
	for _, event := range []*ClientEvent{
		NewOnboardingStepShownEvent("a", 0, 0), NewOnboardingStepCompletedEvent("a", 0, 0), NewOnboardingStepSkippedEvent("a", 0, 0),
		NewOfferScreenShownEvent("intro_step", "e", "v", "standard", 1, "USD", 1), NewOfferCardTappedEvent("yearly"),
		NewOfferCtaTappedEvent("yearly", "apple"), NewOfferDeclinedEvent("back", 1),
		NewPurchaseStartedEvent("apple", "p", "yearly", true, 1, "USD"), NewPurchaseCompletedEvent("apple", "p", "yearly", true, 1, "USD"),
		NewPurchaseCancelledEvent("apple", "p", "yearly", true, 1, "USD"), NewPurchaseFailedEvent("apple", "p", "yearly", true, 1, "USD", "x"),
		NewConnectFirstEvent(), NewWidgetAddedEvent("k"), NewFeedbackSubmittedEvent(3, "r", "t"), NewSignupOptoutChangedEvent(true),
	} {
		connect.AssertEqual(t, nil, checkClientEvent(event))
	}

	names := ClientEventNames()
	connect.AssertEqual(t, 15, names.Len())
	sorted := names.getAll()
	connect.AssertEqual(t, true, sort.StringsAreSorted(sorted))
}

func mustJson(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	connect.AssertEqual(t, nil, err)
	return b
}

// TestParseClientEventsJson pins the web path: the closed schema is enforced
// before anything is sent.
func TestParseClientEventsJson(t *testing.T) {
	events, err := ParseClientEventsJson(`[
		{"name": "onboarding.step.shown", "props": {"step": "plan", "index": 1}},
		{"name": "connect.first"},
		{"name": "feedback.submitted", "at": "2026-09-10T12:00:00Z", "props": {"rating": 5, "has_text": false}}
	]`)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, 3, events.Len())
	connect.AssertEqual(t, "connect.first", events.Get(1).Name)
	connect.AssertEqual(t, true, events.Get(1).At != "")
	connect.AssertEqual(t, "2026-09-10T12:00:00Z", events.Get(2).At)
	connect.AssertEqual(t, `{"step":"plan","index":1}` != "", true)

	_, err = ParseClientEventsJson(`[{"name": "offer.shown"}]`)
	connect.AssertEqual(t, true, err != nil && strings.Contains(err.Error(), "unknown event name"))
	_, err = ParseClientEventsJson(`[{"name": "landing.clicked", "props": {"step": "e1"}}]`)
	connect.AssertEqual(t, true, err != nil)
	_, err = ParseClientEventsJson(`[{"name": "widget.added", "props": {"kind": "globe", "email": "a@b.c"}}]`)
	connect.AssertEqual(t, true, err != nil && strings.Contains(err.Error(), "unknown prop"))
	_, err = ParseClientEventsJson(`not json`)
	connect.AssertEqual(t, true, err != nil)
	_, err = ParseClientEventsJson(`[null]`)
	connect.AssertEqual(t, true, err != nil)
}

type fakeEventSender struct {
	mutex   sync.Mutex
	batches [][]*ClientEvent
	fail    int
	failAll bool
}

func (self *fakeEventSender) send(events []*ClientEvent, callback func(*ClientEventsSendResult, error)) {
	self.mutex.Lock()
	copied := make([]*ClientEvent, len(events))
	copy(copied, events)
	self.batches = append(self.batches, copied)
	shouldFail := self.failAll || 0 < self.fail
	if 0 < self.fail {
		self.fail -= 1
	}
	self.mutex.Unlock()
	if shouldFail {
		callback(nil, errors.New("network down"))
		return
	}
	callback(&ClientEventsSendResult{Accepted: len(events)}, nil)
}

func (self *fakeEventSender) batchCount() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return len(self.batches)
}

// TestClientEventQueue pins the queue: stamping, batching at 200, persistence
// across restarts, holding without a jwt, three retries then drop.
func TestClientEventQueue(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, clientEventQueueFile)
	sender := &fakeEventSender{}
	hasJwt := true
	ctx := context.Background()

	q := newClientEventQueue(ctx, sender.send, func() bool { return hasJwt }, statePath, EventPlatformAndroid, "2026.9.1", "en-US", time.Hour)
	for i := 0; i < 450; i++ {
		q.Add(NewOnboardingStepShownEvent("plan", i, -1))
	}
	connect.AssertEqual(t, 450, q.PendingCount())
	// persisted before any send
	data, err := os.ReadFile(statePath)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, true, 0 < len(data))

	q.FlushAndWait(5000)
	connect.AssertEqual(t, 0, q.PendingCount())
	connect.AssertEqual(t, 3, sender.batchCount())
	connect.AssertEqual(t, 200, len(sender.batches[0]))
	connect.AssertEqual(t, 200, len(sender.batches[1]))
	connect.AssertEqual(t, 50, len(sender.batches[2]))
	first := sender.batches[0][0]
	connect.AssertEqual(t, EventPlatformAndroid, first.Platform)
	connect.AssertEqual(t, "2026.9.1", first.AppVersion)
	connect.AssertEqual(t, "en-US", first.Locale)
	connect.AssertEqual(t, q.GetSession(), first.Session)
	connect.AssertEqual(t, true, strings.HasPrefix(first.Session, "s_"))
	// the state file is gone once empty
	_, err = os.Stat(statePath)
	connect.AssertEqual(t, true, os.IsNotExist(err))
	q.Close()

	// held without a jwt, then persisted and reloaded by a new queue
	hasJwt = false
	sender = &fakeEventSender{}
	q = newClientEventQueue(ctx, sender.send, func() bool { return hasJwt }, statePath, EventPlatformIos, "1", "de", time.Hour)
	q.Add(NewConnectFirstEvent())
	q.Add(NewWidgetAddedEvent("globe"))
	q.FlushAndWait(200)
	connect.AssertEqual(t, 2, q.PendingCount())
	connect.AssertEqual(t, 0, sender.batchCount())
	q.Close()

	hasJwt = true
	q = newClientEventQueue(ctx, sender.send, func() bool { return hasJwt }, statePath, EventPlatformIos, "1", "de", time.Hour)
	connect.AssertEqual(t, 2, q.PendingCount())
	q.FlushAndWait(5000)
	connect.AssertEqual(t, 0, q.PendingCount())
	connect.AssertEqual(t, 1, sender.batchCount())
	connect.AssertEqual(t, EventConnectFirst, sender.batches[0][0].Name)
	connect.AssertEqual(t, `{"kind":"globe"}`, sender.batches[0][1].PropsJson())
	q.Close()

	// three failed calls drop the batch; a later success sends the rest
	sender = &fakeEventSender{fail: 3}
	q = newClientEventQueue(ctx, sender.send, func() bool { return true }, statePath, EventPlatformWeb, "1", "en", time.Hour)
	q.Add(NewOfferCardTappedEvent(PlanYearly))
	q.flushOnce()
	connect.AssertEqual(t, 1, q.PendingCount())
	q.flushOnce()
	connect.AssertEqual(t, 1, q.PendingCount())
	q.flushOnce()
	connect.AssertEqual(t, 0, q.PendingCount())
	connect.AssertEqual(t, 3, sender.batchCount())
	q.Add(NewOfferCardTappedEvent(PlanMonthly))
	q.FlushAndWait(5000)
	connect.AssertEqual(t, 0, q.PendingCount())
	connect.AssertEqual(t, 4, sender.batchCount())
	q.Close()

	// a corrupt state file is discarded
	connect.AssertEqual(t, nil, os.WriteFile(statePath, []byte("{not json"), 0600))
	q = newClientEventQueue(ctx, sender.send, func() bool { return true }, statePath, EventPlatformWeb, "1", "en", time.Hour)
	connect.AssertEqual(t, 0, q.PendingCount())
	q.Close()
}

// TestEventSessionIdRotatesWithinAMillisecond pins that NewSession changes the
// id even when called back to back: ids are time-prefixed, so the session id
// must come from the random tail, not the clock prefix.
func TestEventSessionIdRotatesWithinAMillisecond(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i += 1 {
		id := newEventSessionId()
		if len(id) != 2+16 || id[:2] != "s_" {
			t.Fatalf("unexpected session id shape: %q", id)
		}
		if seen[id] {
			t.Fatalf("session id repeated: %q", id)
		}
		seen[id] = true
	}
}
