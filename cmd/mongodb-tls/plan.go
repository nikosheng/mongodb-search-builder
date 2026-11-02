package main

import (
	"context"
	"fmt"
)

// runPlan prints a read-only summary of the target deployment: which
// processes mongodb-tls will touch, the resolved on-host file paths, and the
// staged rollout order. It performs no writes to Cloud/Ops Manager or to any
// host.
func runPlan(ctx context.Context, c *Config) error {
	client, err := newClient(c)
	if err != nil {
		return err
	}
	cfg, _, err := client.GetAutomationConfig(ctx)
	if err != nil {
		return fmt.Errorf("fetch automation config: %w", err)
	}
	known := map[string]bool{}
	for _, p := range cfg.Processes {
		known[p.Name] = true
	}
	fmt.Printf("deployment: %s (topology=%s)\n", c.Deployment.Name, c.Deployment.Topology)
	fmt.Println("pki.mode:", c.PKI.Mode)
	fmt.Println()
	fmt.Println("process plan:")
	var missing []string
	for _, h := range c.Hosts {
		certPath, err := renderCertPath(c.TLS.CertFileOnHostsTemplate, h.ProcessName)
		if err != nil {
			return err
		}
		status := "found in automation config"
		if !known[h.ProcessName] {
			status = "NOT FOUND in automation config"
			missing = append(missing, h.ProcessName)
		}
		fmt.Printf("  %-12s host=%-28s cert=%-40s ca=%-30s (%s)\n", h.ProcessName, h.Hostname, certPath, c.TLS.CAFileOnHosts, status)
	}
	fmt.Println()
	fmt.Println("rollout order: allowTLS -> preferTLS -> requireTLS, pausing", c.TLS.Rollout.PauseBetweenStages, "between stages")
	fmt.Println("allowConnectionsWithoutCertificates:", *c.TLS.AllowConnectionsWithoutCertificates)
	if len(missing) > 0 {
		return fmt.Errorf("%d process name(s) in hosts[] are not present in the automation config: %v; check deployment.name and processName against Cloud/Ops Manager", len(missing), missing)
	}
	return nil
}
