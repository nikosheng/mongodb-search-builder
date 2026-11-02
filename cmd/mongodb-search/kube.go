package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const ownerKey = "search-cli.mongodb.dev/owner"

type App struct {
	c       *Config
	k       kubernetes.Interface
	d       dynamic.Interface
	getter  genericclioptions.RESTClientGetter
	timeout time.Duration
}

func newApp(c *Config) (*App, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: c.Context}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	rest, err := cfg.ClientConfig()
	if err != nil {
		return nil, err
	}
	k, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return nil, err
	}
	d, err := dynamic.NewForConfig(rest)
	if err != nil {
		return nil, err
	}
	flags := genericclioptions.NewConfigFlags(true)
	flags.Context = &c.Context
	t, _ := time.ParseDuration(c.Timeout)
	return &App{c: c, k: k, d: d, getter: flags, timeout: t}, nil
}
func (a *App) owner() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(a.c.Namespace+"/"+a.c.Name)))[:32]
}
func obj(api, kind, namespace, name string, spec map[string]interface{}) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": api, "kind": kind, "metadata": map[string]interface{}{"name": name}, "spec": spec}}
	if namespace != "" {
		o.SetNamespace(namespace)
	}
	return o
}
func (a *App) resource(gvr schema.GroupVersionResource, ns string) dynamic.ResourceInterface {
	if ns == "" {
		return a.d.Resource(gvr)
	}
	return a.d.Resource(gvr).Namespace(ns)
}
func (a *App) apply(ctx context.Context, gvr schema.GroupVersionResource, o *unstructured.Unstructured) error {
	r := a.resource(gvr, o.GetNamespace())
	old, err := r.Get(ctx, o.GetName(), metav1.GetOptions{})
	if err == nil && old.GetLabels()[ownerKey] != a.owner() {
		return fmt.Errorf("refusing to overwrite %s/%s: not owned by this deployment", o.GetKind(), o.GetName())
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	o.SetLabels(map[string]string{ownerKey: a.owner()})
	b, err := json.Marshal(o.Object)
	if err != nil {
		return err
	}
	force := true
	_, err = r.Patch(ctx, o.GetName(), types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: "mongodb-search-cli", Force: &force})
	if err != nil {
		return fmt.Errorf("apply %s/%s: %w", o.GetKind(), o.GetName(), err)
	}
	return nil
}
func (a *App) await(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, ready func(*unstructured.Unstructured) bool) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		o, e := a.resource(gvr, ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			return false, nil
		}
		if e != nil {
			return false, e
		}
		return ready(o), nil
	})
	if err != nil {
		return fmt.Errorf("wait for %s/%s: %w", gvr.Resource, name, err)
	}
	return nil
}
func condition(o *unstructured.Unstructured) bool {
	cs, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, v := range cs {
		if c, ok := v.(map[string]interface{}); ok && c["type"] == "Ready" && c["status"] == "True" {
			return true
		}
	}
	return false
}
func phase(p string) func(*unstructured.Unstructured) bool {
	return func(o *unstructured.Unstructured) bool {
		s, _, _ := unstructured.NestedString(o.Object, "status", "phase")
		return s == p
	}
}

var (
	cmG     = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secretG = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	svcG    = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	nsG     = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	issuerG = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}
	certG   = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	dbG     = schema.GroupVersionResource{Group: "mongodb.com", Version: "v1", Resource: "mongodb"}
	searchG = schema.GroupVersionResource{Group: "mongodb.com", Version: "v1", Resource: "mongodbsearch"}
	userG   = schema.GroupVersionResource{Group: "mongodb.com", Version: "v1", Resource: "mongodbusers"}
)

func (a *App) helmConfig(ns string) (*action.Configuration, error) {
	ac := new(action.Configuration)
	err := ac.Init(a.getter, ns, "secret", func(string, ...interface{}) {})
	return ac, err
}
func (a *App) helmInstall(ctx context.Context, ns, releaseName, chart, version string, values map[string]interface{}) error {
	ac, err := a.helmConfig(ns)
	if err != nil {
		return err
	}
	get := action.NewGet(ac)
	existing, err := get.Run(releaseName)
	if err == nil {
		if existing.Info.Description != a.releaseOwner() {
			return fmt.Errorf("Helm release %s already exists and is not owned by this deployment", releaseName)
		}
		if existing.Info.Status != release.StatusDeployed {
			return fmt.Errorf("Helm release %s is %s; resolve it before retrying", releaseName, existing.Info.Status)
		}
		return nil
	}
	if err != nil && err.Error() != "release: not found" {
		return err
	}
	inst := action.NewInstall(ac)
	inst.ReleaseName = releaseName
	inst.Namespace = ns
	inst.Wait = true
	inst.Timeout = a.timeout
	inst.Version = version
	inst.Description = a.releaseOwner()
	registryClient, err := registry.NewClient()
	if err != nil {
		return err
	}
	inst.SetRegistryClient(registryClient)
	settings := cli.New()
	path, err := inst.ChartPathOptions.LocateChart(chart, settings)
	if err != nil {
		return err
	}
	ch, err := loader.Load(path)
	if err != nil {
		return err
	}
	_, err = inst.RunWithContext(ctx, ch, values)
	if err != nil {
		return fmt.Errorf("install Helm release %s: %w", releaseName, err)
	}
	return nil
}

func (a *App) releaseOwner() string { return "mongodb-search-cli:" + a.c.Namespace + "/" + a.c.Name }
