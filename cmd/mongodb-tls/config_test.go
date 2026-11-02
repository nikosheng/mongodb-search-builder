package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExampleConfig(t *testing.T) {
	t.Setenv("MDB_CM_API_PRIVATE_KEY", "secret")
	c, err := loadConfig("../../examples/mongodb-tls.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Deployment.Name != "rs0" || len(c.Hosts) != 3 {
		t.Fatalf("unexpected config: %+v", c.Deployment)
	}
	key, err := c.resolvePrivateKey()
	if err != nil || key != "secret" {
		t.Fatalf("resolvePrivateKey() = %q, %v", key, err)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const baseValidConfig = `
cloudManager:
  url: https://cloud.mongodb.com
  orgId: org1
  projectId: proj1
  publicKey: pub
  privateKey: priv
deployment:
  topology: replicaSet
  name: rs0
hosts:
  - processName: rs0_1
    hostname: mongo1.example.com
    sshUser: mongod
    sshKey: /tmp/key
    targetDir: /etc/ssl/mongodb
`

func TestValidateRejectsShardedCluster(t *testing.T) {
	path := writeConfig(t, `
cloudManager: {url: https://cloud.mongodb.com, orgId: o, projectId: p, publicKey: k, privateKey: s}
deployment: {topology: shardedCluster, name: sc0}
hosts:
  - {processName: a, hostname: h, sshUser: u, sshKey: /tmp/k, targetDir: /etc/ssl/mongodb}
`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected shardedCluster topology to be rejected in phase 1")
	}
}

func TestValidateRequiresHosts(t *testing.T) {
	path := writeConfig(t, `
cloudManager: {url: https://cloud.mongodb.com, orgId: o, projectId: p, publicKey: k, privateKey: s}
deployment: {name: rs0}
`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected missing hosts to be rejected")
	}
}

func TestValidateDuplicateProcessName(t *testing.T) {
	path := writeConfig(t, `
cloudManager: {url: https://cloud.mongodb.com, orgId: o, projectId: p, publicKey: k, privateKey: s}
deployment: {name: rs0}
hosts:
  - {processName: a, hostname: h1, sshUser: u, sshKey: /tmp/k, targetDir: /etc/ssl/mongodb}
  - {processName: a, hostname: h2, sshUser: u, sshKey: /tmp/k, targetDir: /etc/ssl/mongodb}
`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected duplicate processName to be rejected")
	}
}

func TestValidateImportModeRequiresCerts(t *testing.T) {
	path := writeConfig(t, baseValidConfig+`
pki:
  mode: import
  caFile: /tmp/ca.pem
`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected import mode without certs entry to be rejected")
	}
}

func TestValidateImportModeAccepted(t *testing.T) {
	path := writeConfig(t, baseValidConfig+`
pki:
  mode: import
  caFile: /tmp/ca.pem
  certs:
    rs0_1: {certFile: /tmp/a.crt, keyFile: /tmp/a.key}
`)
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("expected valid import config to load: %v", err)
	}
}

func TestValidateTargetDirMustMatchTemplates(t *testing.T) {
	path := writeConfig(t, `
cloudManager: {url: https://cloud.mongodb.com, orgId: o, projectId: p, publicKey: k, privateKey: s}
deployment: {name: rs0}
hosts:
  - {processName: a, hostname: h, sshUser: u, sshKey: /tmp/k, targetDir: /wrong/dir}
`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected targetDir mismatch with default caFileOnHosts directory to be rejected")
	}
}

func TestValidateRequiresPrivateKey(t *testing.T) {
	path := writeConfig(t, `
cloudManager: {url: https://cloud.mongodb.com, orgId: o, projectId: p, publicKey: k}
deployment: {name: rs0}
hosts:
  - {processName: a, hostname: h, sshUser: u, sshKey: /tmp/k, targetDir: /etc/ssl/mongodb}
`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected missing privateKey/privateKeyEnv to be rejected")
	}
}

func TestDefaultsApplied(t *testing.T) {
	path := writeConfig(t, baseValidConfig)
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.PKI.Mode != "generate" || c.PKI.Dir != "./pki" || c.Timeout != "15m" {
		t.Fatalf("unexpected defaults: %+v", c.PKI)
	}
	if c.Hosts[0].SSHPort != 22 {
		t.Fatalf("expected default SSH port 22, got %d", c.Hosts[0].SSHPort)
	}
	if *c.TLS.AllowConnectionsWithoutCertificates != true {
		t.Fatal("expected allowConnectionsWithoutCertificates to default to true")
	}
}

func TestRenderCertPath(t *testing.T) {
	got, err := renderCertPath("/etc/ssl/mongodb/{{.ProcessName}}.pem", "rs0_1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/etc/ssl/mongodb/rs0_1.pem" {
		t.Fatalf("unexpected rendered path: %s", got)
	}
}
