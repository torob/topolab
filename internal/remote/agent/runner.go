package agent

import (
	"context"

	"github.com/torob/topolab/internal/app"
)

// LocalRunner runs commands through app.App on the local host.
type LocalRunner struct{}

// Run implements Runner.
func (LocalRunner) Run(ctx context.Context, command app.Command, rio RunIO) error {
	a := app.New(rio.Stdout, rio.Stderr)
	a.ExecHost = rio.ExecHost
	a.CaptureSink = rio.CaptureSink
	a.TemplateDigests = rio.TemplateDigests
	a.LockNotice = rio.LockNotice
	return a.Run(ctx, command)
}
