package main

import (
	"context"
	"fmt"
)

// runStatus prints the current net.tls.mode for every configured process and
// the Automation goal-convergence state.
func runStatus(ctx context.Context, c *Config) error {
	client, err := newClient(c)
	if err != nil {
		return err
	}
	cfg, _, err := client.GetAutomationConfig(ctx)
	if err != nil {
		return fmt.Errorf("fetch automation config: %w", err)
	}
	st, err := client.GetAutomationStatus(ctx)
	if err != nil {
		return fmt.Errorf("fetch automation status: %w", err)
	}
	achieved := map[string]int{}
	for _, p := range st.Processes {
		achieved[p.Name] = p.LastGoalVersionAchieved
	}
	fmt.Printf("automation config version=%d goalVersion=%d\n\n", cfg.Version, st.GoalVersion)
	for _, h := range c.Hosts {
		mode := "unknown"
		for _, p := range cfg.Processes {
			if p.Name != h.ProcessName {
				continue
			}
			mode = extractTLSMode(p.Args26)
		}
		converged := achieved[h.ProcessName] >= st.GoalVersion
		fmt.Printf("%-12s host=%-28s tls.mode=%-12s goalAchieved=%v\n", h.ProcessName, h.Hostname, mode, converged)
	}
	return nil
}

func extractTLSMode(args map[string]any) string {
	net, ok := args["net"].(map[string]any)
	if !ok {
		return "disabled"
	}
	tls, ok := net["tls"].(map[string]any)
	if !ok {
		return "disabled"
	}
	mode, _ := tls["mode"].(string)
	if mode == "" {
		return "disabled"
	}
	return mode
}
