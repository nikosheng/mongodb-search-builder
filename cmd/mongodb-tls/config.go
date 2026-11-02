package main

import (
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the mongodb-tls configuration file. Phase 1 supports a single
// replica set; sharded-cluster support is planned for a later phase and is
// rejected here with a clear error.
type Config struct {
	CloudManager struct {
		URL           string `json:"url"`
		OrgID         string `json:"orgId"`
		ProjectID     string `json:"projectId"`
		PublicKey     string `json:"publicKey"`
		PrivateKey    string `json:"privateKey"`
		PrivateKeyEnv string `json:"privateKeyEnv"`
	} `json:"cloudManager"`

	Deployment struct {
		Topology string `json:"topology"` // replicaSet (sharded cluster: phase 2)
		Name     string `json:"name"`
	} `json:"deployment"`

	PKI struct {
		Mode         string            `json:"mode"` // generate | import
		Dir          string            `json:"dir"`
		CASubject    string            `json:"caSubject"`
		KeyAlgorithm string            `json:"keyAlgorithm"` // rsa | ecdsa
		KeySize      int               `json:"keySize"`
		ValidityDays int               `json:"validityDays"`
		MemberAttrs  map[string]string `json:"memberAttributes"` // O, OU
		SAN          struct {
			IncludeLocalhost bool     `json:"includeLocalhost"`
			ExtraDNS         []string `json:"extraDns"`
			ExtraIPs         []string `json:"extraIps"`
		} `json:"san"`
		CAFile string                `json:"caFile"` // import mode
		Certs  map[string]ImportCert `json:"certs"`  // import mode, keyed by process name
	} `json:"pki"`

	Hosts []HostEntry `json:"hosts"`

	TLS struct {
		AllowConnectionsWithoutCertificates *bool    `json:"allowConnectionsWithoutCertificates"`
		DisabledProtocols                   []string `json:"disabledProtocols"`
		CAFileOnHosts                       string   `json:"caFileOnHosts"`
		CertFileOnHostsTemplate             string   `json:"certFileOnHostsTemplate"`
		Rollout                             struct {
			PauseBetweenStages string `json:"pauseBetweenStages"`
			RequireHealthy     *bool  `json:"requireHealthy"`
		} `json:"rollout"`
	} `json:"tls"`

	Rollback struct {
		SnapshotDir string `json:"snapshotDir"`
	} `json:"rollback"`

	Timeout string `json:"timeout"`
}

// ImportCert is one externally issued certificate/key pair, used in
// pki.mode=import.
type ImportCert struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
}

// HostEntry maps one automation-config process name to its host and SSH
// access details.
type HostEntry struct {
	ProcessName string `json:"processName"`
	Hostname    string `json:"hostname"`
	SSHUser     string `json:"sshUser"`
	SSHPort     int    `json:"sshPort"`
	SSHKey      string `json:"sshKey"`
	TargetDir   string `json:"targetDir"`
	Owner       string `json:"owner"`
	Become      string `json:"become"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err = yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err = c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Deployment.Topology == "" {
		c.Deployment.Topology = "replicaSet"
	}
	if c.PKI.Mode == "" {
		c.PKI.Mode = "generate"
	}
	if c.PKI.Dir == "" {
		c.PKI.Dir = "./pki"
	}
	if c.PKI.CASubject == "" {
		c.PKI.CASubject = "MongoDB TLS CA"
	}
	if c.PKI.KeyAlgorithm == "" {
		c.PKI.KeyAlgorithm = "rsa"
	}
	if c.PKI.KeySize == 0 {
		c.PKI.KeySize = 3072
	}
	if c.PKI.ValidityDays == 0 {
		c.PKI.ValidityDays = 397
	}
	if c.TLS.AllowConnectionsWithoutCertificates == nil {
		t := true
		c.TLS.AllowConnectionsWithoutCertificates = &t
	}
	if c.TLS.CAFileOnHosts == "" {
		c.TLS.CAFileOnHosts = "/etc/ssl/mongodb/db-ca.pem"
	}
	if c.TLS.CertFileOnHostsTemplate == "" {
		c.TLS.CertFileOnHostsTemplate = "/etc/ssl/mongodb/{{.ProcessName}}.pem"
	}
	if c.TLS.Rollout.PauseBetweenStages == "" {
		c.TLS.Rollout.PauseBetweenStages = "10m"
	}
	if c.TLS.Rollout.RequireHealthy == nil {
		t := true
		c.TLS.Rollout.RequireHealthy = &t
	}
	if c.Rollback.SnapshotDir == "" {
		c.Rollback.SnapshotDir = "./backup"
	}
	if c.Timeout == "" {
		c.Timeout = "15m"
	}
	for i := range c.Hosts {
		if c.Hosts[i].SSHPort == 0 {
			c.Hosts[i].SSHPort = 22
		}
	}
}

func (c *Config) validate() error {
	if c.Deployment.Topology != "replicaSet" {
		return fmt.Errorf("deployment.topology %q is not supported yet; mongodb-tls currently supports replicaSet only (sharded cluster support is planned)", c.Deployment.Topology)
	}
	if c.Deployment.Name == "" {
		return fmt.Errorf("deployment.name is required (the replica set _id as shown in Cloud/Ops Manager)")
	}
	if c.CloudManager.URL == "" || c.CloudManager.ProjectID == "" || c.CloudManager.PublicKey == "" {
		return fmt.Errorf("cloudManager.url, projectId, and publicKey are required")
	}
	if c.CloudManager.PrivateKey == "" && c.CloudManager.PrivateKeyEnv == "" {
		return fmt.Errorf("cloudManager.privateKey or cloudManager.privateKeyEnv is required")
	}
	if len(c.Hosts) == 0 {
		return fmt.Errorf("at least one entry is required under hosts")
	}
	seen := map[string]bool{}
	for _, h := range c.Hosts {
		if h.ProcessName == "" || h.Hostname == "" || h.SSHUser == "" || h.SSHKey == "" || h.TargetDir == "" {
			return fmt.Errorf("each hosts entry requires processName, hostname, sshUser, sshKey, and targetDir")
		}
		if seen[h.ProcessName] {
			return fmt.Errorf("duplicate hosts entry for processName %q", h.ProcessName)
		}
		seen[h.ProcessName] = true
		if h.Become != "" && h.Become != "sudo" {
			return fmt.Errorf("hosts.%s.become must be empty or \"sudo\"", h.ProcessName)
		}
	}
	switch c.PKI.Mode {
	case "generate":
		if c.PKI.KeyAlgorithm != "rsa" && c.PKI.KeyAlgorithm != "ecdsa" {
			return fmt.Errorf("pki.keyAlgorithm must be rsa or ecdsa")
		}
	case "import":
		if c.PKI.CAFile == "" {
			return fmt.Errorf("pki.caFile is required when pki.mode is import")
		}
		for _, h := range c.Hosts {
			cert, ok := c.PKI.Certs[h.ProcessName]
			if !ok || cert.CertFile == "" || cert.KeyFile == "" {
				return fmt.Errorf("pki.certs.%s with certFile and keyFile is required when pki.mode is import", h.ProcessName)
			}
		}
	default:
		return fmt.Errorf("pki.mode must be generate or import")
	}
	if t, err := time.ParseDuration(c.Timeout); err != nil || t <= 0 {
		return fmt.Errorf("timeout must be a positive duration")
	}
	if _, err := time.ParseDuration(c.TLS.Rollout.PauseBetweenStages); err != nil {
		return fmt.Errorf("tls.rollout.pauseBetweenStages must be a Go duration (e.g. 10m, 0s)")
	}
	if err := checkHostTargetDirs(c); err != nil {
		return err
	}
	return nil
}

// resolvePrivateKey returns the Cloud/Ops Manager private API key, reading
// an environment variable when privateKey is not set inline.
func (c *Config) resolvePrivateKey() (string, error) {
	if c.CloudManager.PrivateKey != "" {
		return c.CloudManager.PrivateKey, nil
	}
	v := os.Getenv(c.CloudManager.PrivateKeyEnv)
	if v == "" {
		return "", fmt.Errorf("environment variable %s is required for cloudManager.privateKeyEnv", c.CloudManager.PrivateKeyEnv)
	}
	return v, nil
}
