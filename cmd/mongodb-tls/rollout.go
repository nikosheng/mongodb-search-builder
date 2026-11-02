package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nikofeng/mongodb-ea-search/internal/automation"
	"github.com/nikofeng/mongodb-ea-search/internal/cmapi"
)

// bumpSnapshotVersion rewrites the "version" field of a saved automation
// config snapshot so it can be resubmitted as a new document.
func bumpSnapshotVersion(raw []byte, version int) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse snapshot: %w", err)
	}
	doc["version"] = version
	return json.Marshal(doc)
}

func newClient(c *Config) (*cmapi.Client, error) {
	key, err := c.resolvePrivateKey()
	if err != nil {
		return nil, err
	}
	return cmapi.New(c.CloudManager.URL, c.CloudManager.ProjectID, c.CloudManager.PublicKey, key), nil
}

// buildFiles resolves the on-host CA and certificate paths for every
// configured host, which must already have been validated to share a common
// directory with hosts[].targetDir (see checkHostTargetDirs).
func buildFiles(c *Config) ([]automation.ProcessTLSFiles, error) {
	var files []automation.ProcessTLSFiles
	for _, h := range c.Hosts {
		certPath, err := renderCertPath(c.TLS.CertFileOnHostsTemplate, h.ProcessName)
		if err != nil {
			return nil, err
		}
		files = append(files, automation.ProcessTLSFiles{
			ProcessName:        h.ProcessName,
			CertificateKeyFile: certPath,
			CAFile:             c.TLS.CAFileOnHosts,
		})
	}
	return files, nil
}

// runRollout patches the automation config to the given stage, saves a
// rollback snapshot of the prior document, and waits for every targeted
// process to converge.
func runRollout(ctx context.Context, c *Config, stage automation.Stage) error {
	client, err := newClient(c)
	if err != nil {
		return err
	}
	_, raw, err := client.GetAutomationConfig(ctx)
	if err != nil {
		return fmt.Errorf("fetch automation config: %w", err)
	}
	if _, err = automation.SaveSnapshot(c.Rollback.SnapshotDir, raw); err != nil {
		return err
	}
	files, err := buildFiles(c)
	if err != nil {
		return err
	}
	newRaw, version, err := automation.ApplyTLSSettings(raw, automation.Options{
		Stage:                               stage,
		AllowConnectionsWithoutCertificates: *c.TLS.AllowConnectionsWithoutCertificates,
		DisabledProtocols:                   c.TLS.DisabledProtocols,
		Files:                               files,
	})
	if err != nil {
		return fmt.Errorf("apply TLS settings: %w", err)
	}
	fmt.Printf("submitting automation config version %d with net.tls.mode=%s on %d process(es)\n", version, stage, len(files))
	if err = client.PutAutomationConfig(ctx, newRaw); err != nil {
		return fmt.Errorf("submit automation config: %w", err)
	}
	fmt.Println("waiting for Automation goal convergence...")
	if err = automation.WaitForConvergence(ctx, client, version, 10*time.Second, 90); err != nil {
		return fmt.Errorf("rollout to %s did not converge: %w (run 'mongodb-tls status' to inspect, or 'mongodb-tls rollback' to revert)", stage, err)
	}
	fmt.Printf("rollout to %s converged\n", stage)
	return nil
}

// runRollback restores the most recent automation config snapshot.
func runRollback(ctx context.Context, c *Config) error {
	client, err := newClient(c)
	if err != nil {
		return err
	}
	snapshotPath, err := automation.LatestSnapshot(c.Rollback.SnapshotDir)
	if err != nil {
		return err
	}
	snapshot, err := automation.LoadSnapshot(snapshotPath)
	if err != nil {
		return err
	}
	cur, _, err := client.GetAutomationConfig(ctx)
	if err != nil {
		return fmt.Errorf("fetch current automation config: %w", err)
	}
	restored, err := bumpSnapshotVersion(snapshot, cur.Version+1)
	if err != nil {
		return err
	}
	fmt.Printf("restoring automation config from %s as version %d\n", snapshotPath, cur.Version+1)
	if err = client.PutAutomationConfig(ctx, restored); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	if err = automation.WaitForConvergence(ctx, client, cur.Version+1, 10*time.Second, 90); err != nil {
		return fmt.Errorf("rollback did not converge: %w", err)
	}
	fmt.Println("rollback converged")
	return nil
}
