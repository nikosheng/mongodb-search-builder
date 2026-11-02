package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nikofeng/mongodb-ea-search/internal/pki"
)

func newTestConfig(t *testing.T, pkiDir string, hosts []HostEntry) *Config {
	t.Helper()
	c := &Config{}
	c.Deployment.Name = "rs0"
	c.Deployment.Topology = "replicaSet"
	c.CloudManager.URL = "https://cloud.mongodb.com"
	c.CloudManager.ProjectID = "proj"
	c.CloudManager.PublicKey = "pub"
	c.CloudManager.PrivateKey = "priv"
	c.Hosts = hosts
	c.applyDefaults()
	c.PKI.Dir = pkiDir
	c.PKI.MemberAttrs = map[string]string{"O": "ExampleCorp", "OU": "MongoDB Server"}
	return c
}

func TestPKIGenerateCreatesCAAndLeaves(t *testing.T) {
	dir := t.TempDir()
	hosts := []HostEntry{
		{ProcessName: "rs0_1", Hostname: "mongo1.example.com", SSHUser: "u", SSHKey: "/tmp/k", TargetDir: "/etc/ssl/mongodb"},
		{ProcessName: "rs0_2", Hostname: "mongo2.example.com", SSHUser: "u", SSHKey: "/tmp/k", TargetDir: "/etc/ssl/mongodb"},
	}
	c := newTestConfig(t, dir, hosts)
	if err := pkiGenerate(c); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"db-ca.pem", "db-ca.key", "rs0_1.pem", "rs0_2.pem"} {
		if !fileExists(filepath.Join(dir, name)) {
			t.Fatalf("expected %s to exist", name)
		}
	}
	// Running generate again should reuse the existing CA rather than
	// creating a new one.
	caBefore, err := os.ReadFile(filepath.Join(dir, "db-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if err = pkiGenerate(c); err != nil {
		t.Fatal(err)
	}
	caAfter, err := os.ReadFile(filepath.Join(dir, "db-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(caBefore) != string(caAfter) {
		t.Fatal("expected the CA to be reused across repeated pki generate runs")
	}
}

func TestPKIImportValidatesAndCopies(t *testing.T) {
	srcDir := t.TempDir()
	ca, err := pki.GenerateCA(pki.CAOptions{Subject: "Test CA"})
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(srcDir, "ca.pem")
	if err = os.WriteFile(caPath, ca.CertPEM, 0o444); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.IssueLeaf(pki.LeafOptions{
		CommonName: "mongo1.example.com", DNSNames: []string{"mongo1.example.com"},
		Organization: "ExampleCorp", OrganizationalUnit: "MongoDB Server", ClientAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(srcDir, "mongo1.crt")
	keyPath := filepath.Join(srcDir, "mongo1.key")
	os.WriteFile(certPath, certPEM, 0o444)
	os.WriteFile(keyPath, keyPEM, 0o600)

	dir := t.TempDir()
	hosts := []HostEntry{{ProcessName: "rs0_1", Hostname: "mongo1.example.com", SSHUser: "u", SSHKey: "/tmp/k", TargetDir: "/etc/ssl/mongodb"}}
	c := newTestConfig(t, dir, hosts)
	c.PKI.Mode = "import"
	c.PKI.CAFile = caPath
	c.PKI.Certs = map[string]ImportCert{"rs0_1": {CertFile: certPath, KeyFile: keyPath}}

	if err = pkiImport(c); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(dir, "db-ca.pem")) || !fileExists(filepath.Join(dir, "rs0_1.pem")) {
		t.Fatal("expected imported CA and leaf PEM to be written")
	}
}

func TestPKIImportRejectsHostnameMismatch(t *testing.T) {
	srcDir := t.TempDir()
	ca, err := pki.GenerateCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(srcDir, "ca.pem")
	os.WriteFile(caPath, ca.CertPEM, 0o444)
	certPEM, keyPEM, err := ca.IssueLeaf(pki.LeafOptions{CommonName: "wrong-host", DNSNames: []string{"wrong-host"}, ClientAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(srcDir, "wrong.crt")
	keyPath := filepath.Join(srcDir, "wrong.key")
	os.WriteFile(certPath, certPEM, 0o444)
	os.WriteFile(keyPath, keyPEM, 0o600)

	dir := t.TempDir()
	hosts := []HostEntry{{ProcessName: "rs0_1", Hostname: "mongo1.example.com", SSHUser: "u", SSHKey: "/tmp/k", TargetDir: "/etc/ssl/mongodb"}}
	c := newTestConfig(t, dir, hosts)
	c.PKI.Mode = "import"
	c.PKI.CAFile = caPath
	c.PKI.Certs = map[string]ImportCert{"rs0_1": {CertFile: certPath, KeyFile: keyPath}}

	if err = pkiImport(c); err == nil {
		t.Fatal("expected hostname mismatch to be rejected")
	}
}
