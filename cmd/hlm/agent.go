package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
)

// agentCommand runs hlm's agent on another Docker host: it connects out to
// hlm and serves it this host's Docker.
//
//	hlm agent --server https://hlm.example.com --token hlme_…
func agentCommand(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	cfg := agent.Config{Version: version, Log: log}
	fs.StringVar(&cfg.Server, "server", env("HLM_AGENT_SERVER", ""), "hlm's address, such as https://hlm.example.com (HLM_AGENT_SERVER)")
	fs.StringVar(&cfg.Token, "token", env("HLM_AGENT_TOKEN", ""), "the enrolment token from hlm's Hosts page, needed the first time only (HLM_AGENT_TOKEN)")
	fs.StringVar(&cfg.DataDir, "data", env("HLM_AGENT_DATA", "/data"), "where the agent keeps its credential (HLM_AGENT_DATA)")
	fs.StringVar(&cfg.Socket, "socket", env("HLM_AGENT_SOCKET", "/var/run/docker.sock"), "Docker's socket on this host (HLM_AGENT_SOCKET)")
	fs.StringVar(&cfg.CAFile, "ca", env("HLM_AGENT_CA", ""), "a CA certificate (PEM) to trust for hlm's certificate, besides the system's (HLM_AGENT_CA)")
	fs.StringVar(&cfg.ProcPath, "proc", env("HLM_HOST_PROC", existing("/host/proc", "/proc")), "the host's procfs, for its metrics: mount /proc there read-only when in a container (HLM_HOST_PROC)")
	fs.StringVar(&cfg.RootPath, "root", env("HLM_HOST_ROOT", existing("/host/rootfs", "")), "where the host's root filesystem is mounted, for its disks and name (HLM_HOST_ROOT)")
	fs.BoolVar(&cfg.AllowHTTP, "allow-http", env("HLM_AGENT_ALLOW_HTTP", "") != "", "allow a plain http:// address, on a network you trust (HLM_AGENT_ALLOW_HTTP)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: hlm agent --server URL [--token TOKEN] [options]")
		fmt.Fprintln(fs.Output(), "Connects this Docker host to hlm: hlm manages its servers through this agent, which dials out to hlm.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected %q", fs.Arg(0))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agent.Run(ctx, cfg)
}

// existing is path if it exists, otherwise fallback.
func existing(path, fallback string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return fallback
}
