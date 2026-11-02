// Package automation builds and applies the TLS-related changes to a Cloud
// Manager / Ops Manager automation config, and polls for goal convergence.
// It edits the configuration as a generic JSON document (rather than a
// fully-typed struct) so that fields this tool does not model are preserved
// unchanged when the document is written back.
package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nikofeng/mongodb-ea-search/internal/cmapi"
)

// Stage is one step of the staged TLS rollout.
type Stage string

const (
	StageAllow   Stage = "allowTLS"
	StagePrefer  Stage = "preferTLS"
	StageRequire Stage = "requireTLS"
)

// ProcessTLSFiles is the set of file paths a single process should use,
// already resolved to their final on-host locations.
type ProcessTLSFiles struct {
	ProcessName        string
	CertificateKeyFile string
	CAFile             string
}

// Options controls how TLS settings are written into every targeted
// process's args2_6.net.tls block.
type Options struct {
	Stage                               Stage
	AllowConnectionsWithoutCertificates bool
	DisabledProtocols                   []string
	Files                               []ProcessTLSFiles
}

// ApplyTLSSettings patches raw (a full automationConfig JSON document) in
// place, setting net.tls for every process named in opts.Files, and bumps
// the document's version. It returns the new raw document and the new
// version number, which callers use to poll automationStatus for
// convergence.
func ApplyTLSSettings(raw []byte, opts Options) ([]byte, int, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, 0, fmt.Errorf("parse automation config: %w", err)
	}
	version, err := bumpVersion(doc)
	if err != nil {
		return nil, 0, err
	}
	processesRaw, ok := doc["processes"].([]any)
	if !ok {
		return nil, 0, fmt.Errorf("automation config has no processes array")
	}
	byName := map[string]ProcessTLSFiles{}
	for _, f := range opts.Files {
		byName[f.ProcessName] = f
	}
	matched := 0
	for _, p := range processesRaw {
		proc, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := proc["name"].(string)
		files, want := byName[name]
		if !want {
			continue
		}
		matched++
		if err = setProcessTLS(proc, files, opts); err != nil {
			return nil, 0, fmt.Errorf("process %s: %w", name, err)
		}
	}
	if matched != len(opts.Files) {
		return nil, 0, fmt.Errorf("expected to update %d processes but matched %d; check process names against the automation config", len(opts.Files), matched)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, 0, err
	}
	return out, version, nil
}

func bumpVersion(doc map[string]any) (int, error) {
	v, ok := doc["version"]
	if !ok {
		return 0, fmt.Errorf("automation config has no version field")
	}
	n, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("automation config version is not a number")
	}
	next := int(n) + 1
	doc["version"] = next
	return next, nil
}

func setProcessTLS(proc map[string]any, files ProcessTLSFiles, opts Options) error {
	args, ok := proc["args2_6"].(map[string]any)
	if !ok {
		args = map[string]any{}
		proc["args2_6"] = args
	}
	net, ok := args["net"].(map[string]any)
	if !ok {
		net = map[string]any{}
		args["net"] = net
	}
	tls, ok := net["tls"].(map[string]any)
	if !ok {
		tls = map[string]any{}
		net["tls"] = tls
	}
	tls["mode"] = string(opts.Stage)
	tls["certificateKeyFile"] = files.CertificateKeyFile
	tls["CAFile"] = files.CAFile
	tls["allowConnectionsWithoutCertificates"] = opts.AllowConnectionsWithoutCertificates
	if len(opts.DisabledProtocols) > 0 {
		tls["disabledProtocols"] = joinProtocols(opts.DisabledProtocols)
	}
	return nil
}

func joinProtocols(protocols []string) string {
	out := ""
	for i, p := range protocols {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

// WaitForConvergence polls automationStatus until every process has reached
// goalVersion, the context is done, or interval*maxAttempts elapses.
func WaitForConvergence(ctx context.Context, client *cmapi.Client, goalVersion int, interval time.Duration, maxAttempts int) error {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		status, err := client.GetAutomationStatus(ctx)
		if err != nil {
			return fmt.Errorf("check automation status: %w", err)
		}
		if ok, _ := status.Converged(goalVersion); ok {
			return nil
		}
		if attempt == maxAttempts-1 {
			_, pending := status.Converged(goalVersion)
			return fmt.Errorf("automation did not converge to version %d after %d attempts; pending processes: %v", goalVersion, maxAttempts, pending)
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// NextStage returns the stage after s, or ("", false) if s is terminal.
func NextStage(s Stage) (Stage, bool) {
	switch s {
	case StageAllow:
		return StagePrefer, true
	case StagePrefer:
		return StageRequire, true
	default:
		return "", false
	}
}
