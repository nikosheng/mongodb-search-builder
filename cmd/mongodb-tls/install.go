package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nikofeng/mongodb-ea-search/internal/automation"
)

// runInstall runs plan, pki, distribute, and the allowTLS rollout stage
// unconditionally. With auto=true it continues through preferTLS and
// requireTLS automatically, pausing tls.rollout.pauseBetweenStages between
// each stage so clients have time to migrate. Without auto, it stops after
// allowTLS and prints the commands to run next, since moving to requireTLS
// without first confirming every client uses TLS can break production
// traffic.
func runInstall(ctx context.Context, c *Config, yes, auto bool) error {
	if !yes {
		return fmt.Errorf("install changes live Automation configuration and copies files to production hosts; pass --yes to confirm")
	}
	fmt.Println("==> plan")
	if err := runPlan(ctx, c); err != nil {
		return err
	}
	fmt.Println("\n==> pki", c.PKI.Mode)
	var err error
	if c.PKI.Mode == "import" {
		err = pkiImport(c)
	} else {
		err = pkiGenerate(c)
	}
	if err != nil {
		return err
	}
	fmt.Println("\n==> distribute")
	if err = runDistribute(c); err != nil {
		return err
	}
	fmt.Println("\n==> rollout allowTLS")
	if err = runRollout(ctx, c, automation.StageAllow); err != nil {
		return err
	}
	if !auto {
		fmt.Println("\nallowTLS is live. Before continuing, verify every client connects over TLS and trusts", c.TLS.CAFileOnHosts)
		fmt.Println("Then run:")
		fmt.Println("  mongodb-tls rollout prefer -config <file> --yes")
		fmt.Println("  mongodb-tls rollout require -config <file> --yes")
		return nil
	}
	pause, err := time.ParseDuration(c.TLS.Rollout.PauseBetweenStages)
	if err != nil {
		return err
	}
	fmt.Printf("\npausing %s before preferTLS (confirm clients are using TLS)\n", pause)
	if err = sleepOrCancel(ctx, pause); err != nil {
		return err
	}
	fmt.Println("\n==> rollout preferTLS")
	if err = runRollout(ctx, c, automation.StagePrefer); err != nil {
		return err
	}
	fmt.Printf("\npausing %s before requireTLS\n", pause)
	if err = sleepOrCancel(ctx, pause); err != nil {
		return err
	}
	fmt.Println("\n==> rollout requireTLS")
	if err = runRollout(ctx, c, automation.StageRequire); err != nil {
		return err
	}
	fmt.Println("\nTLS is now required. Use", c.TLS.CAFileOnHosts, "as external.databaseCAFile in mongodb-search.")
	return nil
}

func sleepOrCancel(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
