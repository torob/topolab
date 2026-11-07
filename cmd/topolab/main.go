package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/torob/topolab/internal/app"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "agent" && os.Args[2] == "--stdio" {
		os.Exit(agentMain())
	}
	if len(os.Args) == 3 && os.Args[1] == "__job" {
		os.Exit(jobMain(os.Args[2]))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) > 1 && os.Args[1] == "__serial-collector" {
		os.Exit(app.RunSerialCollector(ctx, os.Args[2:], os.Stderr))
	}
	if len(os.Args) > 3 && os.Args[1] == "__topolab-exec-child" && os.Args[2] == "--" {
		os.Exit(app.RunExecChild(os.NewFile(3, "topolab-exec-config"), os.Args[3:], os.Stderr))
	}
	runtime := app.New(os.Stdout, os.Stderr)
	runtime.Stdin = os.Stdin
	// Interrupt handling belongs to each runtime: the local one cancels its
	// context, and the remote client interprets the signals itself.
	os.Exit((CLI{Runtime: runtimeAdapter{runtime}, Remote: remoteRunner{}, Installer: agentInstaller{}}).Execute(context.Background(), os.Args[1:]))
}
