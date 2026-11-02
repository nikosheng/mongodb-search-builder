package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

type ResourceQuantities struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type Resources struct {
	Requests ResourceQuantities `json:"requests"`
	Limits   ResourceQuantities `json:"limits"`
}

type Config struct {
	Context                 string    `json:"context"`
	Namespace               string    `json:"namespace"`
	Name                    string    `json:"name"`
	Mode                    string    `json:"mode"`
	Version                 string    `json:"version"`
	Members                 int       `json:"members"`
	SearchReplicas          int       `json:"searchReplicas"`
	MongoDBResources        Resources `json:"mongodbResources"`
	SearchResources         Resources `json:"searchResources"`
	Timeout                 string    `json:"timeout"`
	OperatorChartVersion    string    `json:"operatorChartVersion"`
	CertManagerChartVersion string    `json:"certManagerChartVersion"`
	OpsManager              struct {
		URL        string `json:"url"`
		OrgID      string `json:"orgId"`
		Project    string `json:"project"`
		PublicKey  string `json:"publicKey"`
		PrivateKey string `json:"privateKey"`
		UserEnv    string `json:"userEnv"`
		APIKeyEnv  string `json:"apiKeyEnv"`
	} `json:"opsManager"`
	AdminPassword    string `json:"adminPassword"`
	SyncPassword     string `json:"syncPassword"`
	AdminPasswordEnv string `json:"adminPasswordEnv"`
	SyncPasswordEnv  string `json:"syncPasswordEnv"`
	External         struct {
		Hosts               []string `json:"hosts"`
		Hostname            string   `json:"hostname"`
		DatabaseCAFile      string   `json:"databaseCAFile"`
		SearchCAFile        string   `json:"searchCAFile"`
		SearchCertFile      string   `json:"searchCertFile"`
		SearchKeyFile       string   `json:"searchKeyFile"`
		EnvoyCertFile       string   `json:"envoyCertFile"`
		EnvoyKeyFile        string   `json:"envoyKeyFile"`
		EnvoyClientCertFile string   `json:"envoyClientCertFile"`
		EnvoyClientKeyFile  string   `json:"envoyClientKeyFile"`
	} `json:"external"`
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
	if c.Namespace == "" {
		c.Namespace = "mongodb"
	}
	if c.Name == "" {
		c.Name = "mdb-rs"
	}
	if c.Version == "" {
		c.Version = "8.3.4-ent"
	}
	if c.Members == 0 {
		c.Members = 3
	}
	if c.SearchReplicas == 0 {
		c.SearchReplicas = 1
	}
	if c.Timeout == "" {
		c.Timeout = "15m"
	}
	if err = c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	dns := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	if !dns.MatchString(c.Name) || len(c.Name) > 32 || !dns.MatchString(c.Namespace) || len(c.Namespace) > 63 {
		return fmt.Errorf("name and namespace must be DNS labels; name must be at most 32 characters")
	}
	if c.Mode != "managed" && c.Mode != "external" {
		return fmt.Errorf("mode must be managed or external")
	}
	if c.Context == "" {
		return fmt.Errorf("context is required")
	}
	if c.Members < 1 || c.Members > 50 || c.SearchReplicas < 1 {
		return fmt.Errorf("members must be 1..50 and searchReplicas must be positive")
	}
	if t, err := time.ParseDuration(c.Timeout); err != nil || t <= 0 {
		return fmt.Errorf("timeout must be a positive duration")
	}
	if c.Mode == "external" && c.MongoDBResources != (Resources{}) {
		return fmt.Errorf("mongodbResources applies only to managed mode")
	}
	if err := validateResources("mongodbResources", c.MongoDBResources); err != nil {
		return err
	}
	if err := validateResources("searchResources", effectiveSearchResources(c.SearchResources)); err != nil {
		return err
	}
	if c.SyncPassword == "" && c.SyncPasswordEnv == "" {
		return fmt.Errorf("syncPassword or syncPasswordEnv is required")
	}
	if c.Mode == "managed" {
		v := regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.[0-9]+-ent$`)
		m := v.FindStringSubmatch(c.Version)
		if m == nil {
			return fmt.Errorf("version must be MongoDB Enterprise 8.3 or later (e.g. 8.3.4-ent)")
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if major < 8 || (major == 8 && minor < 3) {
			return fmt.Errorf("version must be MongoDB Enterprise 8.3 or later (e.g. 8.3.4-ent)")
		}
		if c.OpsManager.URL == "" || c.OpsManager.OrgID == "" || c.OpsManager.Project == "" ||
			(c.OpsManager.PublicKey == "" && c.OpsManager.UserEnv == "") ||
			(c.OpsManager.PrivateKey == "" && c.OpsManager.APIKeyEnv == "") ||
			(c.AdminPassword == "" && c.AdminPasswordEnv == "") {
			return fmt.Errorf("managed mode requires opsManager settings and an admin password (inline or environment variable)")
		}
	} else {
		if len(c.External.Hosts) == 0 || c.External.Hostname == "" || c.External.DatabaseCAFile == "" || c.External.SearchCAFile == "" || c.External.SearchCertFile == "" || c.External.SearchKeyFile == "" {
			return fmt.Errorf("external mode requires hosts, hostname, database CA, search CA, search certificate and key")
		}
		if strings.Contains(c.External.Hostname, ":") {
			return fmt.Errorf("external.hostname must be a DNS hostname without port")
		}
		for _, h := range c.External.Hosts {
			if _, _, err := net.SplitHostPort(h); err != nil {
				return fmt.Errorf("external host %q must be host:port", h)
			}
		}
		if c.SearchReplicas > 1 && (c.External.EnvoyCertFile == "" || c.External.EnvoyKeyFile == "" || c.External.EnvoyClientCertFile == "" || c.External.EnvoyClientKeyFile == "") {
			return fmt.Errorf("external multi-replica mode requires Envoy server and client certificate files")
		}
	}
	return nil
}

func effectiveSearchResources(r Resources) Resources {
	if r.Requests.CPU == "" {
		r.Requests.CPU = "2"
	}
	if r.Requests.Memory == "" {
		r.Requests.Memory = "4Gi"
	}
	return r
}

func validateResources(name string, r Resources) error {
	for _, entry := range []struct {
		name, request, limit string
	}{
		{"cpu", r.Requests.CPU, r.Limits.CPU},
		{"memory", r.Requests.Memory, r.Limits.Memory},
	} {
		var request, limit resource.Quantity
		for _, v := range []struct {
			kind, value string
			parsed      *resource.Quantity
		}{{"requests", entry.request, &request}, {"limits", entry.limit, &limit}} {
			if v.value == "" {
				continue
			}
			q, err := resource.ParseQuantity(v.value)
			if err != nil || q.Sign() <= 0 {
				return fmt.Errorf("%s.%s.%s must be a positive Kubernetes quantity", name, v.kind, entry.name)
			}
			*v.parsed = q
		}
		if entry.request != "" && entry.limit != "" && request.Cmp(limit) > 0 {
			return fmt.Errorf("%s.requests.%s must not exceed its limit", name, entry.name)
		}
	}
	return nil
}

func credential(value, name, field string) (string, error) {
	if value != "" {
		return value, nil
	}
	if name == "" {
		return "", fmt.Errorf("%s or its environment variable is required", field)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("environment variable %s is required", name)
	}
	return v, nil
}
