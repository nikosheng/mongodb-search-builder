package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func m(kv ...interface{}) map[string]interface{} {
	x := map[string]interface{}{}
	for i := 0; i < len(kv); i += 2 {
		x[kv[i].(string)] = kv[i+1]
	}
	return x
}
func (a *App) put(ctx context.Context, g schema.GroupVersionResource, api, kind, ns, name string, body map[string]interface{}) error {
	o := obj(api, kind, ns, name, nil)
	delete(o.Object, "spec")
	for k, v := range body {
		o.Object[k] = v
	}
	return a.apply(ctx, g, o)
}
func (a *App) secret(ctx context.Context, name string, data map[string]string) error {
	enc := map[string]interface{}{}
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return a.put(ctx, secretG, "v1", "Secret", a.c.Namespace, name, m("type", "Opaque", "data", enc))
}
func (a *App) configmap(ctx context.Context, name string, data map[string]interface{}) error {
	return a.put(ctx, cmG, "v1", "ConfigMap", a.c.Namespace, name, m("data", data))
}
func (a *App) ensureNamespace(ctx context.Context, ns string) error {
	_, err := a.resource(nsG, "").Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return a.put(ctx, nsG, "v1", "Namespace", "", ns, nil)
}
func (a *App) deployDB(ctx context.Context) error {
	c := a.c
	if c.Mode == "external" {
		fmt.Println("External replica set: deploy-db is not required.")
		return nil
	}
	user, err := credential(c.OpsManager.PublicKey, c.OpsManager.UserEnv, "opsManager.publicKey")
	if err != nil {
		return err
	}
	key, err := credential(c.OpsManager.PrivateKey, c.OpsManager.APIKeyEnv, "opsManager.privateKey")
	if err != nil {
		return err
	}
	admin, err := credential(c.AdminPassword, c.AdminPasswordEnv, "adminPassword")
	if err != nil {
		return err
	}
	sync, err := credential(c.SyncPassword, c.SyncPasswordEnv, "syncPassword")
	if err != nil {
		return err
	}
	if err = a.configmap(ctx, c.Name+"-om-project", m("projectName", c.OpsManager.Project, "baseUrl", c.OpsManager.URL, "orgId", c.OpsManager.OrgID)); err != nil {
		return err
	}
	if err = a.secret(ctx, c.Name+"-om-credentials", map[string]string{"publicKey": user, "privateKey": key, "user": user, "publicApiKey": key}); err != nil {
		return err
	}
	spec := dbSpec(c)
	if err = a.put(ctx, dbG, "mongodb.com/v1", "MongoDB", c.Namespace, c.Name, m("spec", spec)); err != nil {
		return err
	}
	if err = a.await(ctx, dbG, c.Namespace, c.Name, phase("Running")); err != nil {
		return err
	}
	for _, u := range []struct{ name, username, password, role string }{{c.Name + "-admin", "mdb-admin", admin, "root"}, {c.Name + "-search-sync-source", "search-sync-source", sync, "searchCoordinator"}} {
		sn := u.name + "-password"
		if err = a.secret(ctx, sn, map[string]string{"password": u.password}); err != nil {
			return err
		}
		us := m("username", u.username, "db", "admin", "mongodbResourceRef", m("name", c.Name), "passwordSecretKeyRef", m("name", sn, "key", "password"), "roles", []interface{}{m("name", u.role, "db", "admin")})
		if err = a.put(ctx, userG, "mongodb.com/v1", "MongoDBUser", c.Namespace, u.name, m("spec", us)); err != nil {
			return err
		}
		if err = a.await(ctx, userG, c.Namespace, u.name, phase("Updated")); err != nil {
			return err
		}
	}
	return nil
}

func resourceRequirements(r Resources) map[string]interface{} {
	result := m()
	for _, entry := range []struct {
		name   string
		values ResourceQuantities
	}{{"requests", r.Requests}, {"limits", r.Limits}} {
		values := m()
		if entry.values.CPU != "" {
			values["cpu"] = entry.values.CPU
		}
		if entry.values.Memory != "" {
			values["memory"] = entry.values.Memory
		}
		if len(values) > 0 {
			result[entry.name] = values
		}
	}
	return result
}

func dbSpec(c *Config) map[string]interface{} {
	spec := m("type", "ReplicaSet", "members", int64(c.Members), "version", c.Version, "opsManager", m("configMapRef", m("name", c.Name+"-om-project")), "credentials", c.Name+"-om-credentials", "security", m("certsSecretPrefix", "certs", "tls", m("enabled", true, "ca", c.Name+"-ca"), "authentication", m("enabled", true, "ignoreUnknownUsers", true, "modes", []interface{}{"SCRAM"})), "persistent", true)
	if resources := resourceRequirements(c.MongoDBResources); len(resources) > 0 {
		spec["podSpec"] = m("podTemplate", m("spec", m("containers", []interface{}{
			m("name", "mongodb-enterprise-database", "resources", resources),
		})))
	}
	return spec
}

func (a *App) deploySearch(ctx context.Context) error {
	c := a.c
	sync, err := credential(c.SyncPassword, c.SyncPasswordEnv, "syncPassword")
	if err != nil {
		return err
	}
	if err = a.secret(ctx, c.Name+"-search-sync-source-password", map[string]string{"password": sync}); err != nil {
		return err
	}
	spec := searchSpec(c)
	if err = a.put(ctx, searchG, "mongodb.com/v1", "MongoDBSearch", c.Namespace, c.Name, m("spec", spec)); err != nil {
		return err
	}
	if c.Mode == "external" {
		selector := m("app", c.Name+"-search-0-svc")
		if c.SearchReplicas > 1 { // Reuse the operator-created proxy service's selector, without modifying it.
			proxy := c.Name + "-search-0-proxy-svc"
			err = a.await(ctx, svcG, c.Namespace, proxy, func(o *unstructured.Unstructured) bool {
				s, ok, _ := unstructured.NestedMap(o.Object, "spec", "selector")
				return ok && len(s) > 0
			})
			if err != nil {
				return err
			}
			o, err := a.resource(svcG, c.Namespace).Get(ctx, proxy, metav1.GetOptions{})
			if err != nil {
				return err
			}
			selector, _, _ = unstructured.NestedMap(o.Object, "spec", "selector")
		}
		s := m("type", "LoadBalancer", "selector", selector, "ports", []interface{}{m("name", "mongot", "port", int64(27028), "targetPort", int64(27028), "protocol", "TCP")})
		if err = a.put(ctx, svcG, "v1", "Service", c.Namespace, c.Name+"-external", m("spec", s)); err != nil {
			return err
		}
		fmt.Printf("Expose DNS %s to Service %s/%s-external. Configure the external MongoDB servers with mongotHost and searchIndexManagementHostAndPort = %s:27028, searchTLSMode=requireTLS, useGrpcForSearch=true, and skipAuthenticationToSearchIndexManagementServer=false. Trust the Search CA.\n", c.External.Hostname, c.Namespace, c.Name, c.External.Hostname)
	}
	return a.await(ctx, searchG, c.Namespace, c.Name, phase("Running"))
}

func searchSpec(c *Config) map[string]interface{} {
	source := m("mongodbResourceRef", m("name", c.Name))
	cluster := m("replicas", int64(c.SearchReplicas), "resourceRequirements", resourceRequirements(effectiveSearchResources(c.SearchResources)))
	if c.Mode == "external" {
		hosts := make([]interface{}, len(c.External.Hosts))
		for i, h := range c.External.Hosts {
			hosts[i] = h
		}
		source = m("username", "search-sync-source", "passwordSecretRef", m("name", c.Name+"-search-sync-source-password", "key", "password"), "external", m("hostAndPorts", hosts, "tls", m("ca", m("name", c.Name+"-db-ca"))))
	}
	if c.SearchReplicas > 1 {
		lb := m()
		if c.Mode == "external" {
			lb["externalHostname"] = c.External.Hostname
		}
		cluster["loadBalancer"] = m("managed", lb)
	}
	return m("source", source, "security", m("tls", m("certsSecretPrefix", "certs")), "clusters", []interface{}{cluster})
}
func (a *App) init(ctx context.Context) error {
	if _, err := credential(a.c.SyncPassword, a.c.SyncPasswordEnv, "syncPassword"); err != nil {
		return err
	}
	if a.c.Mode == "managed" {
		for _, entry := range []struct{ value, env, field string }{
			{a.c.AdminPassword, a.c.AdminPasswordEnv, "adminPassword"},
			{a.c.OpsManager.PublicKey, a.c.OpsManager.UserEnv, "opsManager.publicKey"},
			{a.c.OpsManager.PrivateKey, a.c.OpsManager.APIKeyEnv, "opsManager.privateKey"},
		} {
			if _, err := credential(entry.value, entry.env, entry.field); err != nil {
				return err
			}
		}
	}
	for _, ns := range []string{a.c.Namespace, "cert-manager"} {
		if err := a.ensureNamespace(ctx, ns); err != nil {
			return err
		}
	}
	// Reuse a conventional existing operator installation without taking ownership.
	_, err := a.k.AppsV1().Deployments(a.c.Namespace).Get(ctx, "mongodb-kubernetes-operator", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if err = a.helmInstall(ctx, a.c.Namespace, "mongodb-kubernetes", "oci://quay.io/mongodb/helm-charts/mongodb-kubernetes", a.c.OperatorChartVersion, nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = a.k.AppsV1().Deployments("cert-manager").Get(ctx, "cert-manager", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if err = a.helmInstall(ctx, "cert-manager", "cert-manager", "oci://quay.io/jetstack/charts/cert-manager", a.c.CertManagerChartVersion, m("crds", m("enabled", true))); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return nil
}
func certSpec(name, issuer string, dns []string, usages []interface{}) map[string]interface{} {
	spec := m("secretName", name, "issuerRef", m("name", issuer, "kind", "ClusterIssuer"), "duration", "8760h", "renewBefore", "720h", "privateKey", m("algorithm", "RSA", "size", int64(2048)), "usages", usages)
	ds := make([]interface{}, len(dns))
	for i, v := range dns {
		ds[i] = v
	}
	if len(ds) > 0 {
		spec["dnsNames"] = ds
	} else {
		spec["commonName"] = name
	}
	return spec
}
func (a *App) cert(ctx context.Context, name string, dns []string, usages []interface{}) error {
	spec := certSpec(name, a.c.Name+"-ca-issuer", dns, usages)
	if err := a.put(ctx, certG, "cert-manager.io/v1", "Certificate", a.c.Namespace, name, m("spec", spec)); err != nil {
		return err
	}
	return a.await(ctx, certG, a.c.Namespace, name, condition)
}
func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), nil
}
func (a *App) externalCert(ctx context.Context, name, certPath, keyPath string, host, ca string, usage x509.ExtKeyUsage) error {
	cert, err := readFile(certPath)
	if err != nil {
		return err
	}
	key, err := readFile(keyPath)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
	if err != nil {
		return fmt.Errorf("certificate %s: %w", certPath, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if host != "" {
		if err = leaf.VerifyHostname(host); err != nil {
			return fmt.Errorf("certificate %s: %w", certPath, err)
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ca)) {
		return fmt.Errorf("invalid search CA")
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		intermediate, e := x509.ParseCertificate(der)
		if e != nil {
			return e
		}
		intermediates.AddCert(intermediate)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return fmt.Errorf("certificate %s is not valid for the provided search CA: %w", certPath, err)
	}
	return a.secret(ctx, name, map[string]string{"tls.crt": cert, "tls.key": key, "ca.crt": ca})
}
func (a *App) certs(ctx context.Context) error {
	c := a.c
	n := c.Name
	ns := c.Namespace
	prefix := "certs-" + n
	if c.Mode == "external" {
		ca, err := readFile(c.External.DatabaseCAFile)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return fmt.Errorf("invalid database CA")
		}
		if err = a.configmap(ctx, n+"-db-ca", m("ca.crt", ca)); err != nil {
			return err
		}
		searchCA, err := readFile(c.External.SearchCAFile)
		if err != nil {
			return err
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(searchCA)) {
			return fmt.Errorf("invalid search CA")
		}
		if err = a.secret(ctx, n+"-search-ca", map[string]string{"ca.crt": searchCA}); err != nil {
			return err
		}
		host := c.External.Hostname
		if c.SearchReplicas > 1 {
			host = ""
		}
		if err = a.externalCert(ctx, prefix+"-search-cert", c.External.SearchCertFile, c.External.SearchKeyFile, host, searchCA, x509.ExtKeyUsageServerAuth); err != nil {
			return err
		}
		if c.SearchReplicas > 1 {
			if err = a.externalCert(ctx, prefix+"-search-lb-0-cert", c.External.EnvoyCertFile, c.External.EnvoyKeyFile, c.External.Hostname, searchCA, x509.ExtKeyUsageServerAuth); err != nil {
				return err
			}
			if err = a.externalCert(ctx, prefix+"-search-lb-0-client-cert", c.External.EnvoyClientCertFile, c.External.EnvoyClientKeyFile, "", searchCA, x509.ExtKeyUsageClientAuth); err != nil {
				return err
			}
		}
		return nil
	}
	bootstrap := n + "-bootstrap"
	caName := n + "-root-ca"
	issuer := n + "-ca-issuer"
	if err := a.put(ctx, issuerG, "cert-manager.io/v1", "ClusterIssuer", "", bootstrap, m("spec", m("selfSigned", m()))); err != nil {
		return err
	}
	if err := a.await(ctx, issuerG, "", bootstrap, condition); err != nil {
		return err
	}
	if err := a.put(ctx, certG, "cert-manager.io/v1", "Certificate", "cert-manager", caName, m("spec", m("isCA", true, "commonName", caName, "secretName", caName, "privateKey", m("algorithm", "ECDSA", "size", int64(256)), "issuerRef", m("name", bootstrap, "kind", "ClusterIssuer")))); err != nil {
		return err
	}
	if err := a.await(ctx, certG, "cert-manager", caName, condition); err != nil {
		return err
	}
	if err := a.put(ctx, issuerG, "cert-manager.io/v1", "ClusterIssuer", "", issuer, m("spec", m("ca", m("secretName", caName)))); err != nil {
		return err
	}
	if err := a.await(ctx, issuerG, "", issuer, condition); err != nil {
		return err
	}
	ca, err := a.k.CoreV1().Secrets("cert-manager").Get(ctx, caName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	certBytes := ca.Data["tls.crt"]
	if len(certBytes) == 0 {
		return fmt.Errorf("CA secret has no tls.crt")
	}
	if err = a.configmap(ctx, n+"-ca", m("ca-pem", string(certBytes), "ca.crt", string(certBytes))); err != nil {
		return err
	}
	if err = a.secret(ctx, n+"-ca", map[string]string{"ca.crt": string(certBytes)}); err != nil {
		return err
	}
	both := []interface{}{"server auth", "client auth"}
	if err = a.cert(ctx, prefix+"-cert", []string{"*." + n + "-svc." + ns + ".svc.cluster.local", n + "-svc." + ns + ".svc.cluster.local"}, both); err != nil {
		return err
	}
	if err = a.cert(ctx, prefix+"-search-cert", []string{"*." + n + "-search-0-svc." + ns + ".svc.cluster.local", n + "-search-0-svc." + ns + ".svc.cluster.local"}, both); err != nil {
		return err
	}
	if c.SearchReplicas > 1 {
		dns := []string{n + "-search-0-proxy-svc." + ns + ".svc.cluster.local"}
		if err = a.cert(ctx, prefix+"-search-lb-0-cert", dns, []interface{}{"server auth"}); err != nil {
			return err
		}
		if err = a.cert(ctx, prefix+"-search-lb-0-client-cert", nil, []interface{}{"client auth"}); err != nil {
			return err
		}
	}
	return nil
}
