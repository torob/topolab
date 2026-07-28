package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
)

var commandNames = map[string]bool{
	"config": true, "up": true, "ps": true, "logs": true,
	"capture": true, "counters": true, "start": true, "stop": true,
	"restart": true, "down": true,
	"exec": true,
}

// RuntimeCommand is the validated, normalized handoff to lifecycle packages.
type RuntimeCommand struct {
	Name       string
	Args       []string
	Topology   *config.Config
	WorkingDir string
}

type Runtime interface {
	Run(context.Context, RuntimeCommand) error
}

type runtimeAdapter struct{ app *app.App }

func (r runtimeAdapter) Run(ctx context.Context, command RuntimeCommand) error {
	return r.app.Run(ctx, app.Command{Name: command.Name, Args: command.Args, Topology: command.Topology, WorkingDir: command.WorkingDir})
}

type CLI struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv func(string) (string, bool)
	EUID      func() int
	Runtime   Runtime
}

func (c CLI) Execute(ctx context.Context, args []string) int {
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if c.LookupEnv == nil {
		c.LookupEnv = os.LookupEnv
	}
	if c.EUID == nil {
		c.EUID = os.Geteuid
	}
	fs := flag.NewFlagSet("topolab", flag.ContinueOnError)
	fs.SetOutput(c.Stderr)
	file := fs.String("file", "topology.yml", "topology YAML file")
	fs.StringVar(file, "f", "topology.yml", "topology YAML file")
	working := fs.String("working-dir", "", "working directory")
	fs.Usage = func() {
		fmt.Fprintln(c.Stderr, "usage: topolab [-f FILE|--file FILE] [--working-dir DIR] COMMAND [OPTIONS] [NODE ...]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	name := rest[0]
	if !commandNames[name] {
		fmt.Fprintf(c.Stderr, "topolab: unknown command %q\n", name)
		fs.Usage()
		return 2
	}
	wd := *working
	if strings.TrimSpace(wd) == "" {
		wd, _ = c.LookupEnv("WORKING_DIR")
	}
	result, err := config.Load(config.LoadOptions{File: *file, WorkingDir: wd, LookupEnv: c.LookupEnv})
	if err != nil {
		fmt.Fprintf(c.Stderr, "topolab: %v\n", err)
		return 1
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(c.Stderr, "warning: %s\n", warning)
	}
	if name == "config" {
		if len(rest) != 1 {
			fmt.Fprintln(c.Stderr, "topolab: config accepts no arguments")
			return 2
		}
		data, err := config.Marshal(result.Config)
		if err != nil {
			fmt.Fprintf(c.Stderr, "topolab: encode normalized config: %v\n", err)
			return 1
		}
		if _, err := c.Stdout.Write(data); err != nil {
			fmt.Fprintf(c.Stderr, "topolab: write output: %v\n", err)
			return 1
		}
		return 0
	}
	if c.EUID() != 0 {
		fmt.Fprintf(c.Stderr, "topolab: %s requires effective UID 0\n", name)
		return 1
	}
	if c.Runtime == nil {
		fmt.Fprintf(c.Stderr, "topolab: command %q is not implemented\n", name)
		return 1
	}
	if err := c.Runtime.Run(ctx, RuntimeCommand{Name: name, Args: append([]string(nil), rest[1:]...), Topology: result.Config, WorkingDir: result.WorkingDir}); err != nil {
		var childExit *app.ChildExitError
		if errors.As(err, &childExit) {
			return childExit.Code
		}
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintf(c.Stderr, "topolab: %v\n", err)
		}
		return 1
	}
	return 0
}
