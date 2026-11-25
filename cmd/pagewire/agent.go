package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/ThePhaseless/pagewire/internal/agent"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

const agentUsage = `usage: pagewire agent probe|fetch|run [flags]

commands:
  probe  write the USB inventory as JSON
  fetch  download, verify and install accepted vendor blobs
  run    report to the control plane and apply the desired state

Run "pagewire agent <command> --help" for its flags.
`

// agentCmd runs `agent probe|fetch|run` (the Agent CLI contract).
func agentCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(strings.TrimSpace(agentUsage))
	}

	fl := flag.NewFlagSet("agent "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "probe":
		out := fl.String("out", device.InventoryFile, "inventory JSON output file")
		sysRoot := fl.String("sys-root", "/sys", "sysfs root")
		devRoot := fl.String("dev-root", "/dev", "device node root")
		var sel config.USB
		fl.StringVar(&sel.VendorID, "vendor-id", "", "select a device: vendor id (4 hex)")
		fl.StringVar(&sel.ProductID, "product-id", "", "select a device: product id (4 hex)")
		fl.StringVar(&sel.Serial, "serial", "", "select a device: serial")
		if err := parseAgentFlags(fl, args[1:]); err != nil {
			return err
		}
		var selp *config.USB
		if sel.VendorID != "" || sel.ProductID != "" {
			selp = &sel
		}
		return agent.WriteJSON(*out, agent.ProbeFile{Inventory: agent.Probe(*sysRoot, *devRoot, selp), Nodes: agent.ProbeNodes(*devRoot)})
	case "fetch":
		var c agent.FetchConfig
		fl.StringVar(&c.Dir, "dir", "", "blob volume")
		fl.Var((*strList)(&c.Blobs), "blob", "blob to install, <id>@<version> (repeatable)")
		fl.Var((*strList)(&c.Accept), "accept", "accepted EULA, <id>@<version>@<sha256 of the accepted licence text> (repeatable)")
		fl.Var((*strList)(&c.Optional), "optional", "blob id whose failure is not fatal (repeatable)")
		fl.StringVar(&c.StatusOut, "status-out", device.BlobStatusFile, "blob status JSON output file")
		if err := parseAgentFlags(fl, args[1:]); err != nil {
			return err
		}
		if c.Dir == "" {
			return errors.New("agent fetch needs --dir")
		}
		return agent.Fetch(ctx, c)
	case "run":
		var c agent.Config
		fl.StringVar(&c.ControlPlaneURL, "control-plane-url", "", "control-plane internal URL")
		fl.StringVar(&c.TokenFile, "token-file", "", "bearer token file")
		fl.StringVar(&c.PodUID, "pod-uid", "", "this pod's UID")
		fl.StringVar(&c.Workload, strings.TrimPrefix(device.AgentWorkloadArg, "--"), "", "this workload's node name (Spec.Node); empty for a single workload")
		fl.StringVar(&c.InventoryFile, "inventory", "", "inventory JSON from agent probe")
		fl.StringVar(&c.BlobStatusFile, "blob-status", "", "blob status JSON from agent fetch, re-read every cycle")
		fl.StringVar(&c.BlobDir, "blob-dir", "", "blob volume where vendor archives are staged to show their licence text; empty: licence texts report failed")
		fl.StringVar(&c.SysRoot, "sys-root", "/sys", "sysfs root")
		fl.StringVar(&c.DBusAddress, "dbus-address", device.DBusAddress, "D-Bus system bus for Avahi; empty disables DNS-SD browsing")
		fl.DurationVar(&c.Interval, "interval", 0, "report interval (default 5s)")
		if err := parseAgentFlags(fl, args[1:]); err != nil {
			return err
		}
		return agent.Run(ctx, c)
	default:
		return fmt.Errorf("unknown agent command %q (want probe, fetch or run)", args[0])
	}
}

func parseAgentFlags(fl *flag.FlagSet, args []string) error {
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", fl.Name(), fl.Args())
	}
	return nil
}
