package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/torob/topolab/internal/app"
)

func main() {
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
	os.Exit((CLI{Runtime: runtimeAdapter{runtime}}).Execute(ctx, os.Args[1:]))
}
