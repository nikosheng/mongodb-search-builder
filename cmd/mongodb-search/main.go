package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"helm.sh/helm/v3/pkg/action"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: mongodb-search <install|init|certs|deploy-db|deploy-search|status|uninstall> -config FILE [--yes]")
	}
	command := os.Args[1]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	file := fs.String("config", "config.yaml", "YAML or JSON configuration")
	yes := fs.Bool("yes", false, "confirm full teardown")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	c, err := loadConfig(*file)
	if err != nil {
		return err
	}
	a, err := newApp(c)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "init":
		return a.init(ctx)
	case "certs":
		return a.certs(ctx)
	case "deploy-db":
		return a.deployDB(ctx)
	case "deploy-search":
		return a.deploySearch(ctx)
	case "install":
		for _, stage := range []struct {
			name string
			fn   func(context.Context) error
		}{{"init", a.init}, {"certs", a.certs}, {"deploy-db", a.deployDB}, {"deploy-search", a.deploySearch}} {
			fmt.Println("==>", stage.name)
			if err := stage.fn(ctx); err != nil {
				return fmt.Errorf("%s: %w", stage.name, err)
			}
		}
		return a.status(ctx)
	case "status":
		return a.status(ctx)
	case "uninstall":
		if !*yes {
			return fmt.Errorf("full teardown deletes owned database, search, Helm releases, namespaces and PVCs; pass --yes to confirm")
		}
		return a.uninstall(ctx)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}
func (a *App) status(ctx context.Context) error {
	for _, x := range []struct {
		g     schema.GroupVersionResource
		label string
	}{{dbG, "MongoDB"}, {searchG, "MongoDBSearch"}} {
		if x.g == dbG && a.c.Mode == "external" {
			continue
		}
		o, err := a.resource(x.g, a.c.Namespace).Get(ctx, a.c.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fmt.Printf("%s: absent\n", x.label)
			continue
		}
		if err != nil {
			return err
		}
		p, _, _ := unstructured.NestedString(o.Object, "status", "phase")
		fmt.Printf("%s %s/%s: %s\n", x.label, a.c.Namespace, a.c.Name, p)
	}
	pods, err := a.k.CoreV1().Pods(a.c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		fmt.Printf("Pod %s: %s\n", p.Name, p.Status.Phase)
	}
	if a.c.Mode == "external" {
		s, err := a.k.CoreV1().Services(a.c.Namespace).Get(ctx, a.c.Name+"-external", metav1.GetOptions{})
		if err == nil {
			fmt.Printf("External service ingress: %v\n", s.Status.LoadBalancer.Ingress)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
func (a *App) deleteOwned(ctx context.Context, g schema.GroupVersionResource, ns, name string) error {
	r := a.resource(g, ns)
	o, err := r.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if o.GetLabels()[ownerKey] != a.owner() {
		return fmt.Errorf("refusing to delete unowned %s/%s", g.Resource, name)
	}
	uid := o.GetUID()
	err = r.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
func (a *App) helmUninstall(ns, name string) error {
	ac, err := a.helmConfig(ns)
	if err != nil {
		return err
	}
	rel, err := action.NewGet(ac).Run(name)
	if err != nil {
		if err.Error() == "release: not found" {
			return nil
		}
		return err
	}
	if rel.Info.Description != a.releaseOwner() {
		return fmt.Errorf("refusing to uninstall unowned Helm release %s", name)
	}
	u := action.NewUninstall(ac)
	u.Wait = true
	u.Timeout = a.timeout
	_, err = u.Run(name)
	return err
}
func (a *App) uninstall(ctx context.Context) error {
	c := a.c
	// Do not infer ownership of storage from StatefulSet names alone.
	owned := map[string]bool{}
	for _, entry := range []struct {
		g    schema.GroupVersionResource
		name string
	}{{dbG, c.Name}, {searchG, c.Name}} {
		o, err := a.resource(entry.g, c.Namespace).Get(ctx, entry.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if o.GetLabels()[ownerKey] != a.owner() {
			return fmt.Errorf("refusing to tear down unowned %s/%s", entry.g.Resource, entry.name)
		}
		owned[entry.g.Resource] = true
	}
	// Capture PVCs from the actual deployment StatefulSets before their owners are deleted.
	var claims []string
	for _, stsName := range []string{c.Name, c.Name + "-search-0"} {
		if stsName == c.Name && !owned[dbG.Resource] || stsName != c.Name && !owned[searchG.Resource] {
			continue
		}
		sts, err := a.k.AppsV1().StatefulSets(c.Namespace).Get(ctx, stsName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, claim := range sts.Spec.VolumeClaimTemplates {
			replicas := int32(1)
			if sts.Spec.Replicas != nil {
				replicas = *sts.Spec.Replicas
			}
			// Include retained PVCs from a previously scaled-down StatefulSet.
			pvcs, err := a.k.CoreV1().PersistentVolumeClaims(c.Namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			prefix := claim.Name + "-" + stsName + "-"
			for _, pvc := range pvcs.Items {
				if strings.HasPrefix(pvc.Name, prefix) {
					suffix := strings.TrimPrefix(pvc.Name, prefix)
					if _, err := strconv.Atoi(suffix); err == nil {
						claims = append(claims, pvc.Name)
					}
				}
			}
			for i := int32(0); i < replicas; i++ {
				claims = append(claims, fmt.Sprintf("%s-%s-%d", claim.Name, stsName, i))
			}
		}
	}
	for _, v := range []struct {
		g        schema.GroupVersionResource
		ns, name string
	}{{searchG, c.Namespace, c.Name}, {userG, c.Namespace, c.Name + "-admin"}, {userG, c.Namespace, c.Name + "-search-sync-source"}, {dbG, c.Namespace, c.Name}, {svcG, c.Namespace, c.Name + "-external"}} {
		if err := a.deleteOwned(ctx, v.g, v.ns, v.name); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	// Let the operator complete finalizers before uninstalling its Helm release.
	for _, g := range []schema.GroupVersionResource{searchG, dbG} {
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, a.timeout, true, func(ctx context.Context) (bool, error) {
			_, e := a.resource(g, c.Namespace).Get(ctx, c.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(e) {
				return true, nil
			}
			return false, e
		})
		if err != nil {
			return fmt.Errorf("wait for %s deletion: %w", g.Resource, err)
		}
	}
	for _, p := range claims {
		err := a.k.CoreV1().PersistentVolumeClaims(c.Namespace).Delete(ctx, p, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for _, name := range []string{"certs-" + c.Name + "-cert", "certs-" + c.Name + "-search-cert", "certs-" + c.Name + "-search-lb-0-cert", "certs-" + c.Name + "-search-lb-0-client-cert"} {
		var generatedUID *types.UID
		if c.Mode == "managed" {
			cert, e := a.resource(certG, c.Namespace).Get(ctx, name, metav1.GetOptions{})
			if e == nil && cert.GetLabels()[ownerKey] == a.owner() {
				s, e := a.k.CoreV1().Secrets(c.Namespace).Get(ctx, name, metav1.GetOptions{})
				if e == nil {
					u := s.UID
					generatedUID = &u
				} else if !apierrors.IsNotFound(e) {
					return e
				}
			} else if e != nil && !apierrors.IsNotFound(e) {
				return e
			}
		}
		if err := a.deleteOwned(ctx, certG, c.Namespace, name); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if generatedUID != nil {
			err := a.k.CoreV1().Secrets(c.Namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: generatedUID}})
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		if c.Mode == "external" {
			if err := a.deleteOwned(ctx, secretG, c.Namespace, name); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	for _, x := range []struct {
		g        schema.GroupVersionResource
		ns, name string
	}{{certG, "cert-manager", c.Name + "-root-ca"}, {issuerG, "", c.Name + "-ca-issuer"}, {issuerG, "", c.Name + "-bootstrap"}, {cmG, c.Namespace, c.Name + "-ca"}, {cmG, c.Namespace, c.Name + "-db-ca"}, {cmG, c.Namespace, c.Name + "-om-project"}, {secretG, c.Namespace, c.Name + "-ca"}, {secretG, c.Namespace, c.Name + "-search-ca"}, {secretG, c.Namespace, c.Name + "-om-credentials"}, {secretG, c.Namespace, c.Name + "-admin-password"}, {secretG, c.Namespace, c.Name + "-search-sync-source-password"}} {
		if err := a.deleteOwned(ctx, x.g, x.ns, x.name); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	operatorConfig, err := a.helmConfig(c.Namespace)
	if err != nil {
		return err
	}
	operatorRelease, e := action.NewGet(operatorConfig).Run("mongodb-kubernetes")
	if e == nil && operatorRelease.Info.Description == a.releaseOwner() {
		otherDBs, e := a.resource(dbG, c.Namespace).List(ctx, metav1.ListOptions{})
		if e != nil {
			return e
		}
		otherSearch, e := a.resource(searchG, c.Namespace).List(ctx, metav1.ListOptions{})
		if e != nil {
			return e
		}
		if len(otherDBs.Items) > 0 || len(otherSearch.Items) > 0 {
			return fmt.Errorf("operator still manages other resources; retry uninstall after removing them")
		}
		if err := a.helmUninstall(c.Namespace, "mongodb-kubernetes"); err != nil {
			return err
		}
	} else if e != nil && e.Error() != "release: not found" {
		return e
	}
	// Shared cert-manager release may have been installed by another deployment; retain it then.
	ac, err := a.helmConfig("cert-manager")
	if err == nil {
		rel, e := action.NewGet(ac).Run("cert-manager")
		if e == nil && rel.Info.Description == a.releaseOwner() {
			searches, e := a.d.Resource(searchG).List(ctx, metav1.ListOptions{})
			if e != nil {
				return e
			}
			if len(searches.Items) > 0 {
				return fmt.Errorf("cert-manager may still be used by other Search deployments; remove them before uninstalling")
			}
			issuers, e := a.resource(issuerG, "").List(ctx, metav1.ListOptions{})
			if e != nil {
				return e
			}
			for _, issuer := range issuers.Items {
				if issuer.GetLabels()[ownerKey] != a.owner() {
					return fmt.Errorf("cert-manager still has other ClusterIssuers; remove dependencies before uninstalling")
				}
			}
			certificates, e := a.d.Resource(certG).List(ctx, metav1.ListOptions{})
			if e != nil {
				return e
			}
			for _, cert := range certificates.Items {
				if cert.GetLabels()[ownerKey] != a.owner() {
					return fmt.Errorf("cert-manager still has other Certificates; remove dependencies before uninstalling")
				}
			}
			if err = a.helmUninstall("cert-manager", "cert-manager"); err != nil {
				return err
			}
		}
	}
	fmt.Printf("Namespaces %s and cert-manager retained; remove them after checking for unrelated resources.\n", c.Namespace)
	return nil
}
