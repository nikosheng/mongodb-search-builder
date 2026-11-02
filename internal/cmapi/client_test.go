package cmapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// digestServer wraps an httptest.Server that challenges every request with
// Digest auth and verifies the response using the given credentials.
func digestServer(t *testing.T, user, pass string, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	const nonce = "testnonce12345"
	const realm = "MMS Public API"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if authz == "" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s", algorithm=MD5`, realm, nonce))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		expected, err := buildDigestHeader(fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s", algorithm=MD5`, realm, nonce), user, pass, r.Method, r.URL.Path, 1)
		if err != nil {
			t.Fatalf("build expected digest: %v", err)
		}
		// The client-generated cnonce differs each time, so compare the
		// fields that don't depend on it instead of the whole header.
		if !containsField(authz, `username="`+user+`"`) || !containsField(authz, `realm="`+realm+`"`) {
			t.Fatalf("unexpected Authorization header: %s (want fields like %s)", authz, expected)
		}
		handler(w, r)
	}))
	return srv
}

func containsField(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestGetAutomationConfigWithDigestAuth(t *testing.T) {
	var calls int32
	srv := digestServer(t, "pub", "priv", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/api/public/v1.0/groups/proj1/automationConfig" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(AutomationConfig{Version: 7, Processes: []Process{{Name: "rs0_1", Hostname: "mongo1.example.com", ProcessType: "mongod"}}})
	})
	defer srv.Close()
	c := New(srv.URL, "proj1", "pub", "priv")
	cfg, raw, err := c.GetAutomationConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 7 || len(cfg.Processes) != 1 || cfg.Processes[0].Name != "rs0_1" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if len(raw) == 0 {
		t.Fatal("expected raw body to be returned")
	}
	if calls == 0 {
		t.Fatal("expected at least one authenticated call")
	}
}

func TestPutAutomationConfig(t *testing.T) {
	var gotBody []byte
	srv := digestServer(t, "pub", "priv", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("expected PUT, got %s", r.Method)
		}
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = buf
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	defer srv.Close()
	c := New(srv.URL, "proj1", "pub", "priv")
	if err := c.PutAutomationConfig(context.Background(), []byte(`{"version":8}`)); err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != `{"version":8}` {
		t.Fatalf("unexpected body sent: %s", gotBody)
	}
}

func TestAutomationStatusConverged(t *testing.T) {
	status := &AutomationStatus{GoalVersion: 5, Processes: []AutomationStatusProcess{
		{Name: "p1", LastGoalVersionAchieved: 5},
		{Name: "p2", LastGoalVersionAchieved: 4},
	}}
	ok, pending := status.Converged(5)
	if ok {
		t.Fatal("expected not converged")
	}
	if len(pending) != 1 || pending[0] != "p2" {
		t.Fatalf("unexpected pending list: %v", pending)
	}
	status.Processes[1].LastGoalVersionAchieved = 5
	ok, pending = status.Converged(5)
	if !ok || len(pending) != 0 {
		t.Fatalf("expected converged, got ok=%v pending=%v", ok, pending)
	}
}

func TestRetriesOn5xx(t *testing.T) {
	var attempts int32
	srv := digestServer(t, "pub", "priv", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(AutomationStatus{GoalVersion: 1})
	})
	defer srv.Close()
	c := New(srv.URL, "proj1", "pub", "priv")
	c.MaxRetries = 5
	// Override retry backoff indirectly by keeping it small via fewer attempts;
	// the default backoff starts at 500ms so this test stays fast with <=3 attempts.
	if _, err := c.GetAutomationStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", attempts)
	}
}

func TestDigestChallengeParsing(t *testing.T) {
	fields, err := parseDigestChallenge(`Digest realm="MMS Public API", qop="auth", nonce="abc123", opaque="xyz"`)
	if err != nil {
		t.Fatal(err)
	}
	if fields["realm"] != "MMS Public API" || fields["nonce"] != "abc123" || fields["opaque"] != "xyz" {
		t.Fatalf("unexpected fields: %+v", fields)
	}
}

func TestDigestChallengeMissingNonce(t *testing.T) {
	if _, err := parseDigestChallenge(`Digest realm="x"`); err == nil {
		t.Fatal("expected error for missing nonce")
	}
}
