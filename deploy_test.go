package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestExamplesValidate(t *testing.T) {
	for _, p := range []string{"examples/managed.yaml", "examples/external.yaml"} {
		t.Run(p, func(t *testing.T) {
			if _, err := loadConfig(p); err != nil {
				t.Fatal(err)
			}
		})
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
