package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/client"
	"github.com/torob/topolab/internal/remote/wire"
)

var commandNames = map[string]bool{
	"config": true, "up": true, "ps": true, "logs": true,
	"capture": true, "counters": true, "start": true, "stop": true,
	"restart": true, "down": true,
	"exec": true, "router": true, "version": true,
	"agent": true, "jobs": true, "templates": true,
}

// RuntimeCommand is the validated, normalized handoff to lifecycle packages.
type RuntimeCommand struct {
	Name       string
	Args       []string
	Topology   *config.Config
	WorkingDir string
	Helpers    app.HelperOverrides
}

type Runtime interface {
	Run(context.Context, RuntimeCommand) error
}

// RemoteRunner runs a command on a host through its agent (remote mode).
type RemoteRunner interface {
	Run(context.Context, client.Invocation) int
}

// AgentInstaller installs the controller's executable on a host as the agent.
type AgentInstaller interface {
	Install(ctx context.Context, destination string, sshOptions []string, stdin io.Reader, stdout, stderr io.Writer) int
}

type runtimeAdapter struct{ app *app.App }

func (r runtimeAdapter) Run(ctx context.Context, command RuntimeCommand) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return r.app.Run(ctx, app.Command{Name: command.Name, Args: command.Args, Topology: command.Topology, WorkingDir: command.WorkingDir, Helpers: command.Helpers})
}

type CLI struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv func(string) (string, bool)
	EUID      func() int
	Runtime   Runtime
	Remote    RemoteRunner
	Installer AgentInstaller
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

var reconnectPattern = regexp.MustCompile(`^[1-9][0-9]*(ms|s|m|h)$`)

// parseReconnectTimeout accepts a positive integer followed by ms, s, m, or h,
// or the value 0.
func parseReconnectTimeout(value string) (time.Duration, error) {
	if value == "0" {
		return 0, nil
	}
	if !reconnectPattern.MatchString(value) {
		return 0, fmt.Errorf("--reconnect-timeout must be a positive integer followed by ms, s, m, or h, or 0")
	}
	return time.ParseDuration(value)
}

var jobIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// remoteSubcommand maps `jobs` and `templates` subcommands to command names and
// validates their arguments.
func remoteSubcommand(group string, rest []string) (string, []string, error) {
	if len(rest) < 2 {
		return "", nil, fmt.Errorf("%s requires a subcommand", group)
	}
	sub, args := rest[1], rest[2:]
	switch group + " " + sub {
	case "jobs ls":
		if len(args) > 1 || (len(args) == 1 && args[0] != "--all") {
			return "", nil, errors.New("jobs ls accepts only --all")
		}
	case "jobs show", "jobs attach", "jobs cancel":
		if len(args) != 1 || !jobIDPattern.MatchString(args[0]) {
			return "", nil, fmt.Errorf("%s %s requires one job identifier of 32 lowercase hexadecimal digits", group, sub)
		}
	case "jobs prune", "templates ls":
		if len(args) != 0 {
			return "", nil, fmt.Errorf("%s %s accepts no arguments", group, sub)
		}
	case "templates prune":
		if len(args) != 0 && (len(args) != 2 || args[0] != "--min-age") {
			return "", nil, errors.New("templates prune accepts only --min-age D")
		}
	default:
		return "", nil, fmt.Errorf("unknown %s subcommand %q", group, sub)
	}
	return group + "-" + sub, append([]string(nil), args...), nil
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
	qemuSystem := fs.String("qemu-system", "", "qemu-system-x86_64 executable override")
	qemuImg := fs.String("qemu-img", "", "qemu-img executable override")
	lvm := fs.String("lvm", "", "lvm executable override")
	ovmfCode := fs.String("ovmf-code", "", "OVMF code firmware override")
	ovmfVars := fs.String("ovmf-vars", "", "OVMF variables firmware override")
	xorriso := fs.String("xorriso", "", "xorriso executable override")
	remote := fs.String("remote", "", "run on this host through SSH (DESTINATION)")
	local := fs.Bool("local", false, "run on this host even when a remote is configured")
	var sshOptions stringList
	fs.Var(&sshOptions, "ssh-opt", "ssh -o OPTION (repeatable)")
	reconnect := fs.String("reconnect-timeout", "2m", "how long to reconnect to a running remote job (0 disables)")
	fs.Usage = func() {
		fmt.Fprintln(c.Stderr, "usage: topolab [GLOBAL OPTIONS] COMMAND [OPTIONS]")
		fmt.Fprintln(c.Stderr, "       topolab exec [--user USER] [--cwd DIR] -- COMMAND...")
		fmt.Fprintln(c.Stderr, "       topolab router exec NAME [--user USER] [--cwd DIR] -- COMMAND...")
		fmt.Fprintln(c.Stderr, "       topolab agent install")
		fmt.Fprintln(c.Stderr, "       topolab jobs ls|show|attach|cancel|prune")
		fmt.Fprintln(c.Stderr, "       topolab templates ls|prune")
		fmt.Fprintln(c.Stderr, "global options:")
		fs.PrintDefaults()
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
	if name == "version" {
		if len(rest) != 1 {
			fmt.Fprintln(c.Stderr, "topolab: version accepts no arguments")
			return 2
		}
		fmt.Fprintf(c.Stdout, "topolab %s\n", buildVersion())
		return 0
	}
	if name == "router" {
		if len(rest) < 2 || rest[1] != "exec" {
			fmt.Fprintln(c.Stderr, "topolab: router requires the exec subcommand")
			return 2
		}
		if len(rest) < 3 || strings.HasPrefix(rest[2], "-") {
			fmt.Fprintln(c.Stderr, "topolab: router exec requires NAME")
			return 2
		}
		name = "router-exec"
		rest = append([]string{name}, rest[2:]...)
	}
	if name == "jobs" || name == "templates" {
		sub, subArgs, err := remoteSubcommand(name, rest)
		if err != nil {
			fmt.Fprintf(c.Stderr, "topolab: %v\n", err)
			return 2
		}
		name, rest = sub, append([]string{sub}, subArgs...)
	}
	reconnectTimeout, err := parseReconnectTimeout(*reconnect)
	if err != nil {
		fmt.Fprintf(c.Stderr, "topolab: %v\n", err)
		return 2
	}
	if *local && *remote != "" {
		fmt.Fprintln(c.Stderr, "topolab: --local and --remote cannot be combined")
		return 2
	}
	envRemote := ""
	if !*local {
		envRemote, _ = c.LookupEnv("TOPOLAB_REMOTE")
	}
	destination := *remote
	if destination == "" {
		destination = envRemote
	}
	if destination != "" {
		if err := config.ValidateDestination(destination); err != nil {
			fmt.Fprintf(c.Stderr, "topolab: invalid remote destination: %v\n", err)
			return 2
		}
	}
	if name == "agent" {
		return c.agentCommand(ctx, rest, destination, sshOptions)
	}

	wd := *working
	if strings.TrimSpace(wd) == "" {
		wd, _ = c.LookupEnv("WORKING_DIR")
	}
	if strings.TrimSpace(wd) == "" {
		fmt.Fprintln(c.Stderr, "topolab: working directory is required (--working-dir or WORKING_DIR)")
		return 1
	}
	recorder := &client.RecordingLookup{Base: c.LookupEnv}
	result, err := config.LoadTopology(config.LoadOptions{File: *file, LookupEnv: recorder.Lookup})
	if err != nil {
		fmt.Fprintf(c.Stderr, "topolab: %v\n", err)
		return 1
	}
	if destination == "" && !*local && result.Config.Remote != nil {
		destination = result.Config.Remote.SSH
	}
	isRemote := destination != ""
	if isRemote {
		return c.runRemote(ctx, remoteRun{
			name: name, rest: rest, destination: destination, sshOptions: sshOptions,
			reconnect: reconnectTimeout, result: result, recorder: recorder, wd: wd,
			helpers: wire.Helpers{QEMUSystem: *qemuSystem, QEMUImg: *qemuImg, LVM: *lvm, OVMFCode: *ovmfCode, OVMFVars: *ovmfVars, Xorriso: *xorriso},
		})
	}
	if strings.HasPrefix(name, "jobs-") || strings.HasPrefix(name, "templates-") {
		fmt.Fprintf(c.Stderr, "topolab: %s requires remote mode\n", strings.Replace(name, "-", " ", 1))
		return 2
	}
	resolved, err := config.ResolveWorkingDir(wd)
	if err != nil {
		fmt.Fprintf(c.Stderr, "topolab: %v\n", err)
		return 1
	}
	if err := config.CheckWorkingDir(resolved, result.Config); err != nil {
		fmt.Fprintf(c.Stderr, "topolab: %v\n", err)
		return 1
	}
	result.WorkingDir = resolved
	for _, warning := range result.Warnings {
		fmt.Fprintf(c.Stderr, "warning: %s\n", warning)
	}
	if name == "config" {
		return c.printConfig(result.Config, rest)
	}
	if c.EUID() != 0 {
		displayName := strings.ReplaceAll(name, "-", " ")
		fmt.Fprintf(c.Stderr, "topolab: %s requires effective UID 0\n", displayName)
		return 1
	}
	if c.Runtime == nil {
		fmt.Fprintf(c.Stderr, "topolab: command %q is not implemented\n", name)
		return 1
	}
	helpers := app.HelperOverrides{QEMUSystem: *qemuSystem, QEMUImg: *qemuImg, LVM: *lvm, OVMFCode: *ovmfCode, OVMFVars: *ovmfVars, Xorriso: *xorriso}
	if err := c.Runtime.Run(ctx, RuntimeCommand{Name: name, Args: append([]string(nil), rest[1:]...), Topology: result.Config, WorkingDir: result.WorkingDir, Helpers: helpers}); err != nil {
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

func (c CLI) printConfig(cfg *config.Config, rest []string) int {
	if len(rest) != 1 {
		fmt.Fprintln(c.Stderr, "topolab: config accepts no arguments")
		return 2
	}
	data, err := config.Marshal(cfg)
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

// agentCommand handles `agent install`; `agent --stdio` is dispatched by main
// before the CLI runs.
func (c CLI) agentCommand(ctx context.Context, rest []string, destination string, sshOptions []string) int {
	if len(rest) < 2 || rest[1] != "install" {
		fmt.Fprintln(c.Stderr, "topolab: agent requires the install subcommand")
		return 2
	}
	if len(rest) != 2 {
		fmt.Fprintln(c.Stderr, "topolab: agent install accepts no arguments")
		return 2
	}
	if destination == "" {
		fmt.Fprintln(c.Stderr, "topolab: agent install requires --remote or TOPOLAB_REMOTE")
		return 2
	}
	if c.Installer == nil {
		fmt.Fprintln(c.Stderr, "topolab: command \"agent install\" is not implemented")
		return 1
	}
	return c.Installer.Install(ctx, destination, sshOptions, c.Stdin, c.Stdout, c.Stderr)
}

type remoteRun struct {
	name        string
	rest        []string
	destination string
	sshOptions  []string
	reconnect   time.Duration
	result      *config.Result
	recorder    *client.RecordingLookup
	wd          string
	helpers     wire.Helpers
}

// runRemote runs a command through the agent. It never needs root, and `config`
// never contacts the host.
func (c CLI) runRemote(ctx context.Context, r remoteRun) int {
	if !filepath.IsAbs(r.wd) {
		fmt.Fprintln(c.Stderr, "topolab: the working directory must be an absolute path in remote mode")
		return 2
	}
	wd := filepath.Clean(r.wd)
	if wd == string(filepath.Separator) {
		fmt.Fprintln(c.Stderr, "topolab: working directory may not be the filesystem root")
		return 1
	}
	if r.name == "config" {
		for _, warning := range r.result.Warnings {
			fmt.Fprintf(c.Stderr, "warning: %s\n", warning)
		}
		return c.printConfig(r.result.Config, r.rest)
	}
	if c.Remote == nil {
		fmt.Fprintf(c.Stderr, "topolab: command %q is not implemented in remote mode\n", r.name)
		return 1
	}
	return c.Remote.Run(ctx, client.Invocation{
		Destination:      r.destination,
		SSHOptions:       r.sshOptions,
		ReconnectTimeout: r.reconnect,
		Command:          r.name,
		Args:             append([]string(nil), r.rest[1:]...),
		Topology:         r.result.Config,
		TopologyData:     r.result.Data,
		BaseDir:          r.result.BaseDir,
		Env:              r.recorder.Recorded(),
		WorkingDir:       wd,
		Helpers:          r.helpers,
		Stdin:            c.Stdin,
		Stdout:           c.Stdout,
		Stderr:           c.Stderr,
	})
}
