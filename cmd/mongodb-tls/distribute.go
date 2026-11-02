package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/nikofeng/mongodb-ea-search/internal/hosts"
)

// runDistribute copies the CA and each host's certificate/key PEM from
// cfg.PKI.Dir to the target directory on that host over SSH.
func runDistribute(c *Config) error {
	timeout, err := parseTimeoutOrDefault(c.Timeout, 30*time.Second)
	if err != nil {
		return err
	}
	d := hosts.NewDistributor(timeout)
	caPath := filepath.Join(c.PKI.Dir, caCertFile)
	caName := filepath.Base(c.TLS.CAFileOnHosts)

	var failures int
	for _, h := range c.Hosts {
		certPath := filepath.Join(c.PKI.Dir, h.ProcessName+".pem")
		certName, err := renderCertPath(c.TLS.CertFileOnHostsTemplate, h.ProcessName)
		if err != nil {
			return err
		}
		entry := hosts.Entry{
			ProcessName: h.ProcessName,
			Hostname:    h.Hostname,
			SSHUser:     h.SSHUser,
			SSHPort:     h.SSHPort,
			SSHKeyFile:  h.SSHKey,
			TargetDir:   h.TargetDir,
			Owner:       h.Owner,
			Become:      h.Become,
		}
		files := []hosts.File{
			{LocalPath: caPath, RemoteName: caName, Mode: 0o444},
			{LocalPath: certPath, RemoteName: filepath.Base(certName), Mode: 0o400},
		}
		res := d.Distribute(entry, files)
		if res.Err != nil {
			failures++
			fmt.Printf("%s (%s): FAILED: %v\n", h.ProcessName, h.Hostname, res.Err)
			continue
		}
		fmt.Printf("%s (%s): uploaded=%v skipped=%v\n", h.ProcessName, h.Hostname, res.Uploaded, res.Skipped)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d hosts failed; fix SSH access or paths and re-run distribute", failures, len(c.Hosts))
	}
	return nil
}
