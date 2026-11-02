package automation

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SaveSnapshot writes raw automation config JSON to dir, named with a
// timestamp, so a failed or unwanted rollout can be rolled back with
// RestoreSnapshot. It returns the path written.
func SaveSnapshot(dir string, raw []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot directory: %w", err)
	}
	name := fmt.Sprintf("automation-config.%s.json", time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write snapshot %s: %w", path, err)
	}
	return path, nil
}

// LoadSnapshot reads a previously saved automation config snapshot.
func LoadSnapshot(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read snapshot %s: %w", path, err)
	}
	return b, nil
}

// LatestSnapshot returns the path of the most recently written snapshot in
// dir, or an error if none exist.
func LatestSnapshot(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read snapshot directory %s: %w", dir, err)
	}
	var latest string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if latest == "" || e.Name() > latest {
			latest = e.Name()
		}
	}
	if latest == "" {
		return "", fmt.Errorf("no snapshots found in %s", dir)
	}
	return filepath.Join(dir, latest), nil
}
