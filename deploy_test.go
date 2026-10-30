package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestExamplesValidate(t *testing.T) {
	for _, p := range []string{"config.yaml.sample", "examples/managed.yaml", "examples/external.yaml"} {
		t.Run(p, func(t *testing.T) {
			if _, err := loadConfig(p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCredentialSources(t *testing.T) {
	t.Setenv("TEST_SEARCH_CREDENTIAL", "environment-value")
	for _, tt := range []struct {
		name, value, env, want string
		wantError              bool
	}{
		{"inline", "inline-value", "", "inline-value", false},
		{"environment", "", "TEST_SEARCH_CREDENTIAL", "environment-value", false},
		{"inline precedence", "inline-value", "TEST_SEARCH_CREDENTIAL", "inline-value", false},
		{"inline with missing environment", "inline-value", "TEST_SEARCH_MISSING", "inline-value", false},
		{"missing environment", "", "TEST_SEARCH_MISSING", "", true},
		{"no source", "", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := credential(tt.value, tt.env, "testCredential")
			if (err != nil) != tt.wantError || got != tt.want {
				t.Fatalf("got %q, error %v; want %q, error %t", got, err, tt.want, tt.wantError)
			}
		})
	}
}

func TestCredentialConfigValidation(t *testing.T) {
	for _, path := range []string{"config.yaml.sample", "examples/managed.yaml", "examples/external.yaml"} {
		t.Run(path, func(t *testing.T) {
			c, err := loadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			c.SyncPassword, c.SyncPasswordEnv = "", ""
			if err := c.validate(); err == nil {
				t.Fatal("expected missing sync password failure")
			}
		})
	}
	base, err := loadConfig("config.yaml.sample")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		clear func(*Config)
	}{
		{"public key", func(c *Config) { c.OpsManager.PublicKey, c.OpsManager.UserEnv = "", "" }},
		{"private key", func(c *Config) { c.OpsManager.PrivateKey, c.OpsManager.APIKeyEnv = "", "" }},
		{"admin password", func(c *Config) { c.AdminPassword, c.AdminPasswordEnv = "", "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := *base
			tt.clear(&c)
			if err := c.validate(); err == nil {
				t.Fatal("expected missing credential failure")
			}
		})
	}
	// Sources can be mixed and JSON accepts the same inline field names.
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"context":"test","mode":"managed","opsManager":{"url":"https://example.com","orgId":"org","project":"project","publicKey":"pub","apiKeyEnv":"PRIVATE_KEY"},"adminPassword":"admin","syncPasswordEnv":"SYNC_PASSWORD"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil || c.OpsManager.PublicKey != "pub" || c.OpsManager.APIKeyEnv != "PRIVATE_KEY" || c.AdminPassword != "admin" {
		t.Fatalf("mixed JSON config: %#v, error %v", c, err)
	}
}
func TestConfigValidation(t *testing.T) {
	base, err := loadConfig("examples/managed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		edit func(*Config)
	}{
		{"old enterprise version", func(c *Config) { c.Version = "8.2.9-ent" }},
		{"community version", func(c *Config) { c.Version = "8.3.4" }},
		{"invalid context", func(c *Config) { c.Context = "" }},
		{"negative timeout", func(c *Config) { c.Timeout = "-1s" }},
		{"invalid name", func(c *Config) { c.Name = "bad_name" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := *base
			tt.edit(&c)
			if c.validate() == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
	c := *base
	c.Version = "8.10.0-ent"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("mode: managed\nunknownField: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(p); err == nil {
		t.Fatal("expected strict parsing error")
	}
}
func TestSearchSpecVariants(t *testing.T) {
	for _, tt := range []struct {
		mode     string
		replicas int
	}{{"managed", 1}, {"managed", 2}, {"external", 1}, {"external", 2}} {
		t.Run(tt.mode+string(rune('0'+tt.replicas)), func(t *testing.T) {
			c := &Config{Name: "demo", Mode: tt.mode, SearchReplicas: tt.replicas}
			c.External.Hosts = []string{"db.example:27017"}
			c.External.Hostname = "search.example"
			spec := searchSpec(c)
			src := spec["source"].(map[string]interface{})
			cl := spec["clusters"].([]interface{})[0].(map[string]interface{})
			if (src["mongodbResourceRef"] != nil) != (tt.mode == "managed") {
				t.Fatal("incorrect managed source")
			}
			if (src["external"] != nil) != (tt.mode == "external") {
				t.Fatal("incorrect external source")
			}
			if (cl["loadBalancer"] != nil) != (tt.replicas > 1) {
				t.Fatal("incorrect load balancer")
			}
			if tt.mode == "external" && tt.replicas > 1 {
				lb := cl["loadBalancer"].(map[string]interface{})["managed"].(map[string]interface{})
				if lb["externalHostname"] != "search.example" {
					t.Fatal(lb)
				}
			}
		})
	}
}

func TestResourceSpecifications(t *testing.T) {
	c := &Config{Name: "demo", Members: 3, Version: "8.3.4-ent", Mode: "managed", SearchReplicas: 2}
	if _, ok := dbSpec(c)["podSpec"]; ok {
		t.Fatal("database resource overrides should be absent by default")
	}
	searchResources := func(c *Config) interface{} {
		return searchSpec(c)["clusters"].([]interface{})[0].(map[string]interface{})["resourceRequirements"]
	}
	if got, want := searchResources(c), m("requests", m("cpu", "2", "memory", "4Gi")); !reflect.DeepEqual(got, want) {
		t.Fatalf("default search resources = %#v; want %#v", got, want)
	}
	c.MongoDBResources = Resources{Requests: ResourceQuantities{CPU: "500m", Memory: "2Gi"}, Limits: ResourceQuantities{CPU: "2", Memory: "4Gi"}}
	c.SearchResources = Resources{Requests: ResourceQuantities{Memory: "6Gi"}, Limits: ResourceQuantities{CPU: "3", Memory: "8Gi"}}
	db := dbSpec(c)
	container, ok, err := unstructured.NestedSlice(db, "podSpec", "podTemplate", "spec", "containers")
	if err != nil || !ok || len(container) != 1 {
		t.Fatalf("database container = %#v, found %t, error %v", container, ok, err)
	}
	wantContainer := m("name", "mongodb-enterprise-database", "resources", m("requests", m("cpu", "500m", "memory", "2Gi"), "limits", m("cpu", "2", "memory", "4Gi")))
	if !reflect.DeepEqual(container[0], wantContainer) {
		t.Fatalf("database container = %#v; want %#v", container[0], wantContainer)
	}
	wantSearch := m("requests", m("cpu", "2", "memory", "6Gi"), "limits", m("cpu", "3", "memory", "8Gi"))
	for _, mode := range []string{"managed", "external"} {
		c.Mode = mode
		if got := searchResources(c); !reflect.DeepEqual(got, wantSearch) {
			t.Fatalf("%s search resources = %#v; want %#v", mode, got, wantSearch)
		}
	}
}

func TestResourceConfigValidation(t *testing.T) {
	base, err := loadConfig("config.yaml.sample")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name      string
		edit      func(*Config)
		wantError bool
	}{
		{"valid", func(c *Config) {
			c.MongoDBResources = Resources{Requests: ResourceQuantities{CPU: "500m", Memory: "2Gi"}, Limits: ResourceQuantities{CPU: "1", Memory: "4Gi"}}
		}, false},
		{"limit only database", func(c *Config) { c.MongoDBResources.Limits.CPU = "1" }, false},
		{"invalid quantity", func(c *Config) { c.MongoDBResources.Requests.Memory = "4Xi" }, true},
		{"negative", func(c *Config) { c.SearchResources.Requests.CPU = "-1" }, true},
		{"zero", func(c *Config) { c.SearchResources.Limits.Memory = "0" }, true},
		{"request exceeds limit", func(c *Config) {
			c.MongoDBResources = Resources{Requests: ResourceQuantities{CPU: "1500m"}, Limits: ResourceQuantities{CPU: "1"}}
		}, true},
		{"search default exceeds limit", func(c *Config) { c.SearchResources.Limits.Memory = "2Gi" }, true},
		{"search partial override", func(c *Config) {
			c.SearchResources = Resources{Requests: ResourceQuantities{Memory: "1Gi"}, Limits: ResourceQuantities{Memory: "2Gi"}}
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := *base
			tt.edit(&c)
			err := c.validate()
			if (err != nil) != tt.wantError {
				t.Fatalf("validate = %v; want error %t", err, tt.wantError)
			}
		})
	}
	external, err := loadConfig("examples/external.yaml")
	if err != nil {
		t.Fatal(err)
	}
	external.MongoDBResources.Requests.CPU = "1"
	if err = external.validate(); err == nil || !strings.Contains(err.Error(), "managed mode") {
		t.Fatalf("external database resources: %v", err)
	}
}

func TestResourceConfigParsing(t *testing.T) {
	base, err := os.ReadFile("config.yaml.sample")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := string(base) + "\nmongodbResources:\n  requests:\n    cpu: \"500m\"\n    memory: 2Gi\nsearchResources:\n  limits:\n    cpu: \"4\"\n    memory: 8Gi\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.MongoDBResources.Requests.CPU != "500m" || c.SearchResources.Limits.Memory != "8Gi" {
		t.Fatalf("resources were not parsed: %#v %#v", c.MongoDBResources, c.SearchResources)
	}
}

func TestCertificateSpecIdentity(t *testing.T) {
	// The longest valid deployment name must still produce a valid X.509 common name.
	name := "certs-" + strings.Repeat("a", 32) + "-search-lb-0-client-cert"
	client := certSpec(name, "demo-ca-issuer", nil, []interface{}{"client auth"})
	if got := client["commonName"]; got != name || len(name) > 64 {
		t.Fatalf("client commonName = %v, length = %d", got, len(name))
	}
	if _, ok := client["dnsNames"]; ok {
		t.Fatal("client certificate should not contain empty dnsNames")
	}
	server := certSpec("server", "demo-ca-issuer", []string{"search.example.com"}, []interface{}{"server auth"})
	if _, ok := server["commonName"]; ok {
		t.Fatal("server certificate should use DNS names")
	}
	dns, ok := server["dnsNames"].([]interface{})
	if !ok || len(dns) != 1 || dns[0] != "search.example.com" {
		t.Fatalf("server dnsNames = %v", server["dnsNames"])
	}
}
func TestOwnershipRejectsOtherResources(t *testing.T) {
	ctx := context.Background()
	existing := obj("v1", "ConfigMap", "ns", "x", nil)
	existing.SetLabels(map[string]string{ownerKey: "different"})
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), existing)
	a := &App{c: &Config{Name: "demo", Namespace: "ns"}, d: dyn}
	if err := a.put(ctx, cmG, "v1", "ConfigMap", "ns", "x", m("data", m("x", "y"))); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("unexpected result %v", err)
	}
	if err := a.deleteOwned(ctx, cmG, "ns", "x"); err == nil || !strings.Contains(err.Error(), "unowned") {
		t.Fatalf("unexpected result %v", err)
	}
	if _, err := dyn.Resource(cmG).Namespace("ns").Get(ctx, "x", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	b := &App{c: &Config{Name: "demo", Namespace: "different"}}
	if a.owner() == b.owner() {
		t.Fatal("ownership must include namespace")
	}
}
func TestCondition(t *testing.T) {
	o := &unstructured.Unstructured{Object: m("status", m("conditions", []interface{}{m("type", "Ready", "status", "True")}))}
	if !condition(o) {
		t.Fatal("expected ready")
	}
}
