// Command mongodb-tls enables TLS on an existing MongoDB replica set managed
// by Cloud Manager or Ops Manager. It generates or imports a certificate
// authority and per-host certificates, distributes them over SSH, and drives
// a staged allowTLS -> preferTLS -> requireTLS rollout through the
// Automation API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `usage: mongodb-tls <command> -config FILE [--yes]

commands:
  plan              print the processes, file paths, and rollout plan (read-only)
  pki generate      create (or reuse) a private CA and per-host certificates
  pki import        validate externally issued certificates against the configured CA
  distribute        copy the CA and per-host certificate/key to each host over SSH
  rollout allow     set net.tls.mode=allowTLS on every configured process
  rollout prefer    advance to net.tls.mode=preferTLS
  rollout require   advance to net.tls.mode=requireTLS
  status            print the current TLS mode and Automation convergence per process
  verify            open a TLS connection to each host and check its certificate
  rollback          restore the most recent automation config snapshot
  install           run plan, pki, distribute, and the full staged rollout in order
`

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf(usage)
	}
	args := os.Args[1:]
	command := args[0]
	args = args[1:]
	// Two-word subcommands.
	if command == "pki" || command == "rollout" {
		if len(args) == 0 {
			return fmt.Errorf(usage)
		}
		command = command + " " + args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	file := fs.String("config", "mongodb-tls.yaml", "YAML or JSON configuration")
	yes := fs.Bool("yes", false, "confirm mutating actions without an interactive prompt")
	auto := fs.Bool("auto", false, "install: advance through every rollout stage automatically, pausing tls.rollout.pauseBetweenStages between stages")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	c, err := loadConfig(*file)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "plan":
		return runPlan(ctx, c)
	case "pki generate":
		return pkiGenerate(c)
	case "pki import":
		return pkiImport(c)
	case "distribute":
		return runDistribute(c)
	case "rollout allow":
		if err := requireYes(*yes, "rollout allow"); err != nil {
			return err
		}
		return runRollout(ctx, c, "allowTLS")
	case "rollout prefer":
		if err := requireYes(*yes, "rollout prefer"); err != nil {
			return err
		}
		return runRollout(ctx, c, "preferTLS")
	case "rollout require":
		if err := requireYes(*yes, "rollout require"); err != nil {
			return err
		}
		return runRollout(ctx, c, "requireTLS")
	case "status":
		return runStatus(ctx, c)
	case "verify":
		return runVerify(ctx, c)
	case "rollback":
		if err := requireYes(*yes, "rollback"); err != nil {
			return err
		}
		return runRollback(ctx, c)
	case "install":
		return runInstall(ctx, c, *yes, *auto)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, usage)
	}
}

func requireYes(yes bool, action string) error {
	if !yes {
		return fmt.Errorf("%q changes live Automation configuration on a production replica set; pass --yes to confirm", action)
	}
	return nil
}

func parseTimeoutOrDefault(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}
