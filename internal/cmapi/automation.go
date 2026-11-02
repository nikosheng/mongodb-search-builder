package cmapi

import (
	"context"
	"encoding/json"
	"fmt"
)

// AutomationConfig is a partial representation of the Cloud Manager / Ops
// Manager automationConfig document. Only the fields this tool reads or
// writes are modeled; unknown fields are preserved via RawFields so a
// round-tripped document does not lose data the tool does not understand.
type AutomationConfig struct {
	Version     int              `json:"version"`
	Processes   []Process        `json:"processes"`
	ReplicaSets []ReplicaSet     `json:"replicaSets"`
	Sharding    []ShardedCluster `json:"sharding"`
}

// Process is one mongod/mongos process entry in the automation config.
type Process struct {
	Name        string         `json:"name"`
	Hostname    string         `json:"hostname"`
	ProcessType string         `json:"processType"` // mongod | mongos
	Args26      map[string]any `json:"args2_6"`
}

// ReplicaSet lists the member processes of a replica set, including shard
// replica sets and the config server replica set in a sharded cluster.
type ReplicaSet struct {
	ID      string `json:"_id"`
	Members []struct {
		Host string `json:"host"`
	} `json:"members"`
}

// ShardedCluster lists the shard replica-set IDs and config server replica
// set for a sharded cluster.
type ShardedCluster struct {
	Name                   string  `json:"name"`
	Shards                 []Shard `json:"shards"`
	ConfigServerReplicaSet string  `json:"configServerReplica"`
}

// Shard is one shard within a sharded cluster.
type Shard struct {
	ID     string `json:"_id"`
	RSName string `json:"rs"`
}

// GetAutomationConfig fetches the current automation config for the project.
func (c *Client) GetAutomationConfig(ctx context.Context) (*AutomationConfig, []byte, error) {
	raw, status, err := c.do(ctx, "GET", c.url("/automationConfig"), nil)
	if err != nil {
		return nil, nil, err
	}
	if status != 200 {
		return nil, nil, fmt.Errorf("GET automationConfig: unexpected status %d: %s", status, truncate(raw, 500))
	}
	var cfg AutomationConfig
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return nil, nil, fmt.Errorf("parse automationConfig: %w", err)
	}
	return &cfg, raw, nil
}

// PutAutomationConfig writes a full automation config document. Callers
// should always start from a document returned by GetAutomationConfig (as
// raw JSON, patched in place) to avoid dropping fields this tool does not
// model.
func (c *Client) PutAutomationConfig(ctx context.Context, raw []byte) error {
	b, status, err := c.do(ctx, "PUT", c.url("/automationConfig"), raw)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("PUT automationConfig: unexpected status %d: %s", status, truncate(b, 500))
	}
	return nil
}

// AutomationStatus reports per-process goal convergence.
type AutomationStatus struct {
	GoalVersion int                       `json:"goalVersion"`
	Processes   []AutomationStatusProcess `json:"processes"`
}

// AutomationStatusProcess is the convergence state of one process.
type AutomationStatusProcess struct {
	Name                    string   `json:"name"`
	LastGoalVersionAchieved int      `json:"lastGoalVersionAchieved"`
	Plan                    []string `json:"plan"`
}

// GetAutomationStatus fetches the current goal convergence status.
func (c *Client) GetAutomationStatus(ctx context.Context) (*AutomationStatus, error) {
	var status AutomationStatus
	if err := c.get(ctx, "/automationStatus", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// Converged reports whether every process has reached the given goal
// version, along with the names of any processes still behind.
func (s *AutomationStatus) Converged(goalVersion int) (bool, []string) {
	var pending []string
	for _, p := range s.Processes {
		if p.LastGoalVersionAchieved < goalVersion {
			pending = append(pending, p.Name)
		}
	}
	return len(pending) == 0, pending
}
