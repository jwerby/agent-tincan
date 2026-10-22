package policy_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

type wakes struct {
	mu sync.Mutex
	to []string
}

func (w *wakes) Queued(_ context.Context, r envelope.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.to = append(w.to, r.To)
}

func (w *wakes) count(agent string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, to := range w.to {
		if to == agent {
			n++
		}
	}
	return n
}

// End to end through a real relay with no approval.json: an admin invites
// dot-web with --kind dot-web, a teammate's ask to it is held and wakes
// nothing, and the owner's approval queues it and wakes dot-web.
func TestDotHoldAndApproveThroughRelay(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	w := &wakes{}
	m.Server.SetEvents(w)
	admin := m.Client(t, "admin")
	if code, err := admin.InviteKind(t.Context(), "dot-web", onboard.KindDotWeb); err != nil || code == "" {
		t.Fatalf("invite --kind dot-web: %q %v", code, err)
	}
	dot := m.JoinOnMachineOf(t, "instinct", "dot-web")
	if err := admin.SetKind(t.Context(), "dot-web", onboard.KindDotWeb); err != nil {
		t.Fatal(err)
	}
	muse := m.Client(t, "muse")

	req, err := muse.Send(t.Context(), "dot-web", "summarize my inbox", envelope.KindAsk, "", false)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("ask to dot = %q %v", req.Status, err)
	}
	if w.count("dot-web") != 0 || m.Server.QueuedCount("dot-web") != 0 {
		t.Fatal("held request queued or woke the dot")
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	if w.count("dot-web") != 1 || m.Server.QueuedCount("dot-web") != 1 {
		t.Fatalf("approval did not queue and wake the dot: wakes %d queued %d", w.count("dot-web"), m.Server.QueuedCount("dot-web"))
	}
	inbox, err := dot.Poll(t.Context(), time.Millisecond)
	if err != nil || len(inbox.Requests) != 1 || inbox.Requests[0].ID != req.ID {
		t.Fatalf("dot inbox after approval: %+v %v", inbox, err)
	}

	own, err := dot.Send(t.Context(), "grokbot", "what is on the calendar", envelope.KindAsk, "", false)
	if err != nil || own.Status == envelope.StatusHeld {
		t.Fatalf("dot's own ask = %q %v", own.Status, err)
	}
	grok := m.Client(t, "grokbot")
	if _, err := grok.Claim(t.Context(), own.ID); err != nil {
		t.Fatal(err)
	}
	rep, err := grok.Reply(t.Context(), own.ID, "two meetings", envelope.StatusAnswered)
	if err != nil || rep.Status != envelope.StatusHeld {
		t.Fatalf("reply to the dot = %+v %v", rep, err)
	}
	early, err := dot.Get(t.Context(), own.ID, 0)
	if err != nil || early.Status == envelope.StatusAnswered {
		t.Fatalf("reply reached the dot before approval: %+v %v", early, err)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+rep.RequestID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	res, err := dot.Get(t.Context(), own.ID, time.Second)
	if err != nil || res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.Body != "two meetings" {
		t.Fatalf("reply to the dot's own ask: %+v %v", res, err)
	}
}

// End to end: the owner approves a convening ask to the council, and the
// council's member ask to dot-web under it is queued and wakes the dot,
// with no second approval.
func TestCouncilAskToDotQueuedThroughRelay(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	w := &wakes{}
	m.Server.SetEvents(w)
	admin := m.Client(t, "admin")
	for name, kind := range map[string]string{"dot-web": onboard.KindDotWeb, "council": onboard.KindCouncil} {
		if code, err := admin.InviteKind(t.Context(), name, kind); err != nil || code == "" {
			t.Fatalf("invite --kind %s: %q %v", kind, code, err)
		}
	}
	m.JoinOnMachineOf(t, "instinct", "dot-web")
	council := m.JoinOnMachineOf(t, "muse", "council")
	for name, kind := range map[string]string{"dot-web": onboard.KindDotWeb, "council": onboard.KindCouncil} {
		if err := admin.SetKind(t.Context(), name, kind); err != nil {
			t.Fatal(err)
		}
	}

	convening, err := m.Client(t, "grokbot").Send(t.Context(), "council", "which plan is best?", envelope.KindAsk, "", false)
	if err != nil || convening.Status != envelope.StatusHeld {
		t.Fatalf("convening ask = %q %v", convening.Status, err)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+convening.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := council.Claim(t.Context(), convening.ID); err != nil {
		t.Fatal(err)
	}
	answer, err := council.Send(t.Context(), "dot-web", "which plan is best?", envelope.KindAsk, convening.ID, false)
	if err != nil || answer.Status == envelope.StatusHeld {
		t.Fatalf("council answer ask to the dot = %q %v", answer.Status, err)
	}
	if w.count("dot-web") != 1 || m.Server.QueuedCount("dot-web") != 1 {
		t.Fatalf("council ask did not queue and wake the dot: wakes %d queued %d", w.count("dot-web"), m.Server.QueuedCount("dot-web"))
	}
}
