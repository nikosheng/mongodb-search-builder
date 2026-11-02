package main

import (
	"bytes"
	"fmt"
	"path"
	"text/template"
)

// renderCertPath expands tls.certFileOnHostsTemplate (e.g.
// "/etc/ssl/mongodb/{{.ProcessName}}.pem") for one process.
func renderCertPath(tmpl, processName string) (string, error) {
	t, err := template.New("certPath").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse certFileOnHostsTemplate: %w", err)
	}
	var buf bytes.Buffer
	if err = t.Execute(&buf, struct{ ProcessName string }{processName}); err != nil {
		return "", fmt.Errorf("render certFileOnHostsTemplate for %s: %w", processName, err)
	}
	return buf.String(), nil
}

// checkHostTargetDirs verifies that every host's targetDir matches the
// directory implied by the CA and certificate path templates, since the
// SSH distributor writes files under hosts[].targetDir but the Automation
// config must reference those same absolute paths.
func checkHostTargetDirs(c *Config) error {
	caDir := path.Dir(c.TLS.CAFileOnHosts)
	for _, h := range c.Hosts {
		if h.TargetDir != caDir {
			return fmt.Errorf("hosts.%s.targetDir (%s) must match the directory of tls.caFileOnHosts (%s)", h.ProcessName, h.TargetDir, caDir)
		}
		certPath, err := renderCertPath(c.TLS.CertFileOnHostsTemplate, h.ProcessName)
		if err != nil {
			return err
		}
		if certDir := path.Dir(certPath); h.TargetDir != certDir {
			return fmt.Errorf("hosts.%s.targetDir (%s) must match the directory implied by tls.certFileOnHostsTemplate (%s)", h.ProcessName, h.TargetDir, certDir)
		}
	}
	return nil
}
