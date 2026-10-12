package relay_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestReplyAndAnswerWaitForApproval(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}},"notify":"instinct","hold_ttl":"1h"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	wakes := &approvalWakes{}
	m.Server.SetEvents(wakes)
	ctx := t.Context()
	muse, grok, admin := m.Client(t, "muse"), m.Client(t, "grokbot"), m.Client(t, "admin")

	req, err := muse.Send(ctx, "grokbot", "book a table", envelope.KindAsk, "", false)
	if err != nil || req.Status == envelope.StatusHeld {
		t.Fatalf("send to ungated grokbot = %+v %v", req, err)
	}
	if _, err := grok.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	rep, err := grok.Reply(ctx, req.ID, "Which night?", envelope.StatusNeedsInput)
	if err != nil || rep.Status != envelope.StatusHeld || rep.RequestID == "" {
		t.Fatalf("clarification = %+v %v", rep, err)
	}
	if wakes.count("muse") != 0 {
		t.Fatal("held clarification woke muse")
	}
	inbox, err := muse.Poll(ctx, time.Millisecond)
	if err != nil || len(inbox.Requests) != 0 {
		t.Fatalf("asker saw a held clarification: %+v %v", inbox, err)
	}
	got, err := muse.Get(ctx, req.ID, 0)
	if err != nil || got.Status == envelope.StatusNeedsInput {
		t.Fatalf("parent changed before approval: %+v %v", got, err)
	}
	again, err := grok.Reply(ctx, req.ID, "Still which night?", envelope.StatusNeedsInput)
	if err == nil || !client.IsStatus(err, 409) {
		t.Fatalf("second clarification = %+v %v, want 409", again, err)
	}

	if err := admin.Raw(ctx, "POST", "/v1/admin/requests/"+rep.RequestID+"/deny", map[string]string{"reason": "no"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err = muse.Get(ctx, req.ID, 0)
	if err != nil || got.Status != envelope.StatusClaimed || got.Reply != nil {
		t.Fatalf("denied clarification changed the parent: %+v %v", got, err)
	}
	rep, err = grok.Reply(ctx, req.ID, "Which night?", envelope.StatusNeedsInput)
	if err != nil || rep.Status != envelope.StatusHeld {
		t.Fatalf("retry = %+v %v", rep, err)
	}
	if err := admin.Raw(ctx, "POST", "/v1/admin/requests/"+rep.RequestID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err = muse.Get(ctx, req.ID, 0)
	if err != nil || got.Status != envelope.StatusNeedsInput || got.Reply == nil || got.Reply.Body != "Which night?" {
		t.Fatalf("approved clarification = %+v %v", got, err)
	}

	if err := os.WriteFile(path, []byte(`{"gate":{"grokbot":{"from":"*"}},"hold_ttl":"1h"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ans, err := muse.Answer(ctx, req.ID, "Friday")
	if err != nil || ans.Status != envelope.StatusHeld {
		t.Fatalf("answer = %+v %v", ans, err)
	}
	if _, err := grok.Poll(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	queued := m.Server.QueuedCount("grokbot")
	if queued != 0 {
		t.Fatalf("held answer queued %d", queued)
	}
	if err := admin.Raw(ctx, "POST", "/v1/admin/requests/"+ans.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	in, err := grok.Poll(ctx, time.Second)
	if err != nil || len(in.Requests) != 1 || len(in.Requests[0].Exchanges) != 1 || in.Requests[0].Exchanges[0].Answer != "Friday" {
		t.Fatalf("resumed = %+v %v", in, err)
	}

	closed, err := muse.Send(ctx, "instinct", "done", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "instinct").Claim(ctx, closed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "instinct").Reply(ctx, closed.ID, "finished", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "instinct").Reply(ctx, closed.ID, "again", envelope.StatusAnswered); err == nil || !client.IsStatus(err, 409) {
		t.Fatalf("reply to a closed request = %v, want 409", err)
	}
}

func TestReplyToCouncilIsHeldByKind(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	if ok, err := m.Store.SetAgentKind(t.Context(), "muse", "council"); !ok || err != nil {
		t.Fatalf("kind: %v %v", ok, err)
	}
	ctx := t.Context()
	council, member, admin := m.Client(t, "muse"), m.Client(t, "grokbot"), m.Client(t, "admin")
	req, err := council.Send(ctx, "grokbot", "question", envelope.KindAsk, "", false)
	if err != nil || req.Status == envelope.StatusHeld {
		t.Fatalf("ask to an ordinary member = %+v %v", req, err)
	}
	if _, err := member.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	rep, err := member.Reply(ctx, req.ID, "the report", envelope.StatusAnswered)
	if err != nil || rep.Status != envelope.StatusHeld {
		t.Fatalf("reply to council = %+v %v", rep, err)
	}
	got, err := council.Get(ctx, req.ID, 0)
	if err != nil || got.Status == envelope.StatusAnswered {
		t.Fatalf("reply delivered before approval: %+v %v", got, err)
	}
	if err := admin.Raw(ctx, "POST", "/v1/admin/requests/"+rep.RequestID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err = council.Get(ctx, req.ID, 0)
	if err != nil || got.Status != envelope.StatusAnswered || got.Reply == nil || got.Reply.Body != "the report" {
		t.Fatalf("approved reply = %+v %v", got, err)
	}
}
