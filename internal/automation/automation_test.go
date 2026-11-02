package automation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nikofeng/mongodb-ea-search/internal/cmapi"
)

const sampleConfig = `{
  "version": 10,
  "processes": [
    {"name": "rs0_1", "hostname": "mongo1.example.com", "processType": "mongod", "args2_6": {"net": {"port": 27017}}},
    {"name": "rs0_2", "hostname": "mongo2.example.com", "processType": "mongod"},
    {"name": "rs0_3", "hostname": "mongo3.example.com", "processType": "mongod"}
  ],
  "replicaSets": [
    {"_id": "rs0", "members": [{"host": "rs0_1"}, {"host": "rs0_2"}, {"host": "rs0_3"}]}
  ],
  "unrelatedTopLevelField": {"keep": "me"}
}`

func TestApplyTLSSettingsAllProcesses(t *testing.T) {
	files := []ProcessTLSFiles{
		{ProcessName: "rs0_1", CertificateKeyFile: "/etc/ssl/mongodb/rs0_1.pem", CAFile: "/etc/ssl/mongodb/db-ca.pem"},
		{ProcessName: "rs0_2", CertificateKeyFile: "/etc/ssl/mongodb/rs0_2.pem", CAFile: "/etc/ssl/mongodb/db-ca.pem"},
		{ProcessName: "rs0_3", CertificateKeyFile: "/etc/ssl/mongodb/rs0_3.pem", CAFile: "/etc/ssl/mongodb/db-ca.pem"},
	}
	out, version, err := ApplyTLSSettings([]byte(sampleConfig), Options{
		Stage: StageAllow, AllowConnectionsWithoutCertificates: true, DisabledProtocols: []string{"TLS1_0", "TLS1_1"}, Files: files,
	})
	if err != nil {
		t.Fatal(err)
	}
	if version != 11 {
		t.Fatalf("expected version 11, got %d", version)
	}
	var doc map[string]any
	if err = json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["unrelatedTopLevelField"] == nil {
		t.Fatal("expected unrelated fields to be preserved")
	}
	procs := doc["processes"].([]any)
	for _, p := range procs {
		proc := p.(map[string]any)
		tls := proc["args2_6"].(map[string]any)["net"].(map[string]any)["tls"].(map[string]any)
		if tls["mode"] != "allowTLS" {
			t.Fatalf("expected allowTLS, got %v", tls["mode"])
		}
		if tls["allowConnectionsWithoutCertificates"] != true {
			t.Fatalf("expected allowConnectionsWithoutCertificates=true, got %v", tls["allowConnectionsWithoutCertificates"])
		}
		if !strings.Contains(tls["certificateKeyFile"].(string), proc["name"].(string)) {
			t.Fatalf("unexpected certificateKeyFile: %v", tls["certificateKeyFile"])
		}
	}
	// Existing net.port must be preserved for rs0_1.
	first := procs[0].(map[string]any)
	port := first["args2_6"].(map[string]any)["net"].(map[string]any)["port"]
	if port != float64(27017) {
		t.Fatalf("expected port 27017 to be preserved, got %v", port)
	}
}

func TestApplyTLSSettingsRejectsUnknownProcess(t *testing.T) {
	_, _, err := ApplyTLSSettings([]byte(sampleConfig), Options{
		Stage: StageAllow,
		Files: []ProcessTLSFiles{{ProcessName: "does-not-exist", CertificateKeyFile: "x", CAFile: "y"}},
	})
	if err == nil {
		t.Fatal("expected error for a process name not present in the automation config")
	}
}

func TestNextStage(t *testing.T) {
	cases := []struct {
		in   Stage
		want Stage
		ok   bool
	}{
		{StageAllow, StagePrefer, true},
		{StagePrefer, StageRequire, true},
		{StageRequire, "", false},
	}
	for _, c := range cases {
		got, ok := NextStage(c.in)
		if got != c.want || ok != c.ok {
			t.Fatalf("NextStage(%s) = (%s, %v); want (%s, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path, err := SaveSnapshot(dir, []byte(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	latest, err := LatestSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if latest != path {
		t.Fatalf("latest snapshot = %s; want %s", latest, path)
	}
	b, err := LoadSnapshot(latest)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != sampleConfig {
		t.Fatal("snapshot contents did not round-trip")
	}
}

func TestSnapshotFileNamesSort(t *testing.T) {
	dir := t.TempDir()
	p1, err := SaveSnapshot(dir, []byte(`{"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // ensure a distinct second-resolution timestamp
	p2, err := SaveSnapshot(dir, []byte(`{"version":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatal("expected distinct snapshot filenames")
	}
	latest, err := LatestSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if latest != p2 {
		t.Fatalf("expected latest snapshot to be the second write; got %s want %s", latest, p2)
	}
}

func TestWaitForConvergence(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="r", qop="auth", nonce="n", algorithm=MD5`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls++
		achieved := 1
		if calls >= 2 {
			achieved = 2
		}
		json.NewEncoder(w).Encode(map[string]any{
			"goalVersion": 2,
			"processes": []map[string]any{
				{"name": "rs0_1", "lastGoalVersionAchieved": achieved},
			},
		})
	}))
	defer srv.Close()
	client := cmapi.New(srv.URL, "proj", "pub", "priv")
	err := WaitForConvergence(context.Background(), client, 2, 10*time.Millisecond, 5)
	if err != nil {
		t.Fatalf("expected convergence, got %v", err)
	}
}

func TestWaitForConvergenceTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="r", qop="auth", nonce="n", algorithm=MD5`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"goalVersion": 2,
			"processes":   []map[string]any{{"name": "rs0_1", "lastGoalVersionAchieved": 1}},
		})
	}))
	defer srv.Close()
	client := cmapi.New(srv.URL, "proj", "pub", "priv")
	err := WaitForConvergence(context.Background(), client, 2, 5*time.Millisecond, 3)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestSnapshotDirectoryCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	if _, err := SaveSnapshot(dir, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}
