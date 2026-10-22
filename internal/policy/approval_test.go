package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestApprovalPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	req := envelope.Request{From: "safe", To: "muse", Chain: []string{"untrusted", "safe"}}
	if held, err := a.Held(&req); held || err != nil {
		t.Fatalf("missing: %v %v", held, err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"gate":{"muse":{"from":["untrusted"]}},"hold_ttl":"2h"}`)
	if held, err := a.Held(&req); !held || err != nil {
		t.Fatalf("chain: %v %v", held, err)
	}
	req.Chain = []string{"safe"}
	if held, err := a.Held(&req); held || err != nil {
		t.Fatalf("safe: %v %v", held, err)
	}
	write(`{"gate":{"muse":{"unless":["safe"]}}}`)
	if held, err := a.Held(&req); held || err != nil {
		t.Fatalf("unless: %v %v", held, err)
	}
	req.From = "untrusted"
	if held, err := a.Held(&req); !held || err != nil {
		t.Fatalf("sender: %v %v", held, err)
	}
	write(`{"gate":{"muse":{"from":"*"}}}`)
	if held, err := a.Held(&req); !held || err != nil {
		t.Fatalf("wildcard: %v %v", held, err)
	}
	write(`broken`)
	if held, err := a.Held(&req); !held || err == nil {
		t.Fatalf("fail closed: %v %v", held, err)
	}
	if _, err := LoadApproval(path); err == nil {
		t.Fatal("bad startup accepted")
	}
	write(`{"gate":{"muse":{"from":"*"}}}`)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApproval(path); err == nil {
		t.Fatal("public policy accepted")
	}
}

func TestApprovalInvalidPolicies(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"gate":null}`, `{"gate":{"muse":null}}`,
		`{"gate":{"muse":{"from":null}}}`, `{"gate":{"muse":{"unless":null}}}`,
		`{"gate":{"muse":{"from":"*","unless":[]}}}`, `{"gate":{"muse":{"from":"everyone"}}}`,
		`{"gate":{"muse":{"from":["bad name"]}}}`, `{"gate":{"Bad":{"from":"*"}}}`,
		`{"gate":{"*muse":{"from":"*"}}}`, `{"gate":{"muse*":{"from":"*"}}}`, `{"gate":{"**":{"from":"*"}}}`,
		`{"gate":{"*":{"from":["*"]}}}`,
		`{"gate":{},"hold_ttl":"0s"}`, `{"gate":{},"hold_ttl":"bad"}`, `{"gate":{},"notify":"bad name"}`,
		`{"gate":{},"typo":true}`, `{"gate":{}} {}`, `{"gate":{"muse":{"form":"*"}}}`,
		`{"gate":{"muse":{"from":"*","from":[]}}}`, `{"gate":{"muse":{"from":"*"}},"gate":{}}`,
	} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "approval.json")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadApproval(path); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestApprovalWildcardTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"*":{"from":"*"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"muse", "instinct", "grokbot"} {
		req := envelope.Request{From: "grokbot", To: to, Chain: []string{"grokbot"}}
		held, err := a.Held(&req)
		if !held || err != nil {
			t.Fatalf("%s: held %v err %v", to, held, err)
		}
	}
	if err := os.WriteFile(path, []byte("{\"gate\":{\"\\u002a\":{\"from\":\"*\"},\"muse\":{\"from\":[]}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	muse := envelope.Request{From: "grokbot", To: "muse", Chain: []string{"grokbot"}}
	if held, err := a.Held(&muse); held || err != nil {
		t.Fatalf("named entry should override *: held %v err %v", held, err)
	}
	other := envelope.Request{From: "grokbot", To: "instinct", Chain: []string{"grokbot"}}
	if held, err := a.Held(&other); !held || err != nil {
		t.Fatalf("unlisted target: held %v err %v", held, err)
	}
}

func TestApprovalReloadFailureAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	req := envelope.Request{From: "safe", To: "muse", Chain: []string{"safe"}}
	write(`bad`)
	if held, err := a.Held(&req); held || err == nil {
		t.Fatalf("no good copy: %v %v", held, err)
	}
	write(`{"gate":{"muse":{"from":[]}},"hold_ttl":"3h","notify":"grokbot"}`)
	if held, err := a.Held(&req); held || err != nil {
		t.Fatalf("recovery: %v %v", held, err)
	}
	write(`bad again`)
	if held, err := a.Held(&req); !held || err == nil || req.HoldTTL.String() != "3h0m0s" || req.ApprovalNotify != "grokbot" {
		t.Fatalf("last good: %v %v %+v", held, err, req)
	}
	req.To = "other"
	if held, err := a.Held(&req); held || err != nil {
		t.Fatalf("ungated target: %v %v", held, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	req.To = "muse"
	if held, err := a.Held(&req); held || err != nil {
		t.Fatalf("removed: %v %v", held, err)
	}
	write(`bad after removal`)
	if held, err := a.Held(&req); !held || err == nil {
		t.Fatalf("forgot last good copy: %v %v", held, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApproval(path); err == nil {
		t.Fatal("dangling link treated as absent")
	}
}

// An in-place edit that keeps the file's size and modification time must
// still take effect: a gate the owner adds cannot be missed.
func TestApprovalSeesSameSizeSameMtimeEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	write := func(body string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	write(`{"gate":{"aaaa":{"from":"*"}}}`, at)
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	req := envelope.Request{From: "safe", To: "muse", Chain: []string{"safe"}}
	if held, err := a.Held(&req); held || err != nil {
		t.Fatalf("before edit: %v %v", held, err)
	}
	write(`{"gate":{"muse":{"from":"*"}}}`, at) // same length, same mtime
	if held, err := a.Held(&req); !held || err != nil {
		t.Fatalf("edit missed: held=%v err=%v", held, err)
	}
}

// A ping carries no body and does no work, so the gate lets it through;
// ordinary sends to the same target are still held.
func TestApprovalLetsPingsThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, Config{Approval: a})
	ping := envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindPing}
	if err := f.pol.Prepare(t.Context(), &ping); err != nil || ping.Status == envelope.StatusHeld {
		t.Fatalf("ping = %+v, %v", ping, err)
	}
	ask := envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindAsk, Body: "x"}
	if err := f.pol.Prepare(t.Context(), &ask); err != nil || ask.Status != envelope.StatusHeld {
		t.Fatalf("ask = %+v, %v", ask, err)
	}
}

// A send the gate refuses (its policy file broke with no valid copy) gives
// back the urgent slot it took.
func TestApprovalFailureRefundsUrgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`broken`), 0600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, Config{Approval: a, UrgentPerHour: 1})
	req := envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindAsk, Body: "x", Urgent: true}
	if err := f.pol.Prepare(t.Context(), &req); err == nil {
		t.Fatal("broken gate accepted a send")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	req = envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindAsk, Body: "x", Urgent: true}
	if err := f.pol.Prepare(t.Context(), &req); err != nil {
		t.Fatalf("urgent slot not refunded: %v", err)
	}
}
