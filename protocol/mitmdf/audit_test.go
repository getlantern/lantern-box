package mitmdf

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOpenAuditLog_DisabledWhenEmpty(t *testing.T) {
	a, err := openAuditLog("")
	if err != nil {
		t.Fatalf("openAuditLog(\"\"): %v", err)
	}
	if a != nil {
		t.Error("openAuditLog(\"\") returned a non-nil log; want nil for disabled")
	}
	// nil-safe ops.
	a.record(auditRecord{SNI: "x"})
	if err := a.close(); err != nil {
		t.Errorf("nil.close: %v", err)
	}
}

func TestAuditLog_WritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := openAuditLog(path)
	if err != nil {
		t.Fatalf("openAuditLog: %v", err)
	}
	defer a.close()

	a.record(auditRecord{SNI: "vercel.app", FrontedSNI: "nextjs.org", Decision: auditAllow})
	a.record(auditRecord{SNI: "chase.com", Decision: auditDeny, Reason: "deny list"})

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []auditRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r auditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].SNI != "vercel.app" || got[0].FrontedSNI != "nextjs.org" || got[0].Decision != auditAllow {
		t.Errorf("record[0] = %+v", got[0])
	}
	if got[1].Decision != auditDeny || got[1].Reason != "deny list" {
		t.Errorf("record[1] = %+v", got[1])
	}
	for i, r := range got {
		if r.Timestamp.IsZero() {
			t.Errorf("record[%d] missing timestamp", i)
		}
	}
}

func TestAuditLog_ConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := openAuditLog(path)
	if err != nil {
		t.Fatalf("openAuditLog: %v", err)
	}
	defer a.close()

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			a.record(auditRecord{SNI: "x.test", Decision: auditAllow})
		}()
	}
	wg.Wait()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	count := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r auditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d unmarshal: %v", count+1, err)
		}
		count++
	}
	if count != N {
		t.Errorf("got %d records, want %d", count, N)
	}
}
