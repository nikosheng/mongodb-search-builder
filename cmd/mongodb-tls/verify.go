package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/nikofeng/mongodb-ea-search/internal/pki"
)

// runVerify dials every host on its MongoDB port using the generated or
// imported CA and reports whether the TLS handshake and certificate chain
// are valid. It does not check replica-set health or Automation state.
func runVerify(ctx context.Context, c *Config) error {
	client, err := newClient(c)
	if err != nil {
		return err
	}
	cfg, _, err := client.GetAutomationConfig(ctx)
	if err != nil {
		return fmt.Errorf("fetch automation config: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(c.PKI.Dir, caCertFile))
	if err != nil {
		return fmt.Errorf("read CA certificate (run 'pki generate' or 'pki import' first): %w", err)
	}
	timeout, err := parseTimeoutOrDefault(c.Timeout, 10*time.Second)
	if err != nil {
		return err
	}
	ports := map[string]int{}
	for _, p := range cfg.Processes {
		ports[p.Name] = extractPort(p.Args26)
	}
	var failed int
	for _, h := range c.Hosts {
		port := ports[h.ProcessName]
		if port == 0 {
			port = 27017
		}
		addr := net.JoinHostPort(h.Hostname, fmt.Sprintf("%d", port))
		res := pki.VerifyHandshake(ctx, addr, caPEM, timeout)
		fmt.Println(res.Summary())
		if res.Err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d hosts failed TLS verification", failed, len(c.Hosts))
	}
	return nil
}

func extractPort(args map[string]any) int {
	net, ok := args["net"].(map[string]any)
	if !ok {
		return 0
	}
	p, ok := net["port"].(float64)
	if !ok {
		return 0
	}
	return int(p)
}
