package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
)

var commandNames = map[string]bool{
	"config": true, "up": true, "ps": true, "logs": true,
	"capture": true, "counters": true, "start": true, "stop": true,
	"restart": true, "down": true,
	"exec": true, "router": true, "version": true,
	"snapshot": true,
}

// snapshotSubcommands are the subcommands of `topolab snapshot`.
var snapshotSubcommands = map[string]bool{"create": true, "restore": true, "delete": true, "list": true}

// stopHelp is the output of `topolab stop --help`.
const stopHelp = `usage: topolab [GLOBAL OPTIONS] stop [--hard] [NODE...]

Stop the selected VMs, or all VMs when no NODE is given. Routers, switches,
and namespaces keep running. "topolab start" starts the VMs again with the
same disks.

Without --hard, TopoLab stops the VMs one after another. Each VM gets an ACPI
power-button event and has up to 30 seconds to shut down. Then TopoLab sends
SIGTERM and, at the end, SIGKILL. A running exec or router exec command makes
the stop wait.

  --hard  Simulate a power loss of the selected VMs. TopoLab sends SIGTERM to
          all of their QEMU processes in one loop, so all VMs stop at the same
          moment. The guests get no ACPI event and do not shut down. QEMU
          writes its disk data to the host before it exits, so the disk images
          stay consistent, also with cache: unsafe. TopoLab never sends
          SIGKILL. If a QEMU process does not exit within 30 seconds, TopoLab
          prints the VM and its PID, leaves the process running, and exits
          with status 1. A running exec or router exec command does not make
          stop --hard wait.

          Output: a header and one line for each VM, with tab-separated
          columns NAME, RESULT (stopped, not-exited or not-running), PID,
          STOP_TIME (when TopoLab sent SIGTERM) and EXIT_TIME (when QEMU
          exited). Times are UTC with milliseconds.

Data note: like a real power loss, a hard stop loses the data that a guest did
not flush. With cache: unsafe, a hard stop keeps the disk images consistent,
but a host crash or a host power loss can still lose data and break an image.
`

// snapshotHelp is the output of `topolab snapshot --help`.
const snapshotHelp = `usage: topolab [GLOBAL OPTIONS] snapshot create  NAME [NODE...]
       topolab [GLOBAL OPTIONS] snapshot restore NAME [NODE...]
       topolab [GLOBAL OPTIONS] snapshot delete  NAME [NODE...]
       topolab [GLOBAL OPTIONS] snapshot list    [--format table|json]

A snapshot saves the disks and the OVMF variables of VMs, so that you can
bring them back to that point later. It does not save the RAM of the VMs, so
a restore boots the VMs again. Without NODE, the command uses all VMs: all
VMs of the topology for create, and all VMs of the snapshot for restore and
delete. NAME uses lowercase letters, digits, ".", "_", and "-", with at most
63 characters.

Snapshots support file storage (qcow2 internal snapshots) and lvm-thin
storage (read-only thin snapshot LVs). A VM with an lvm-thick disk cannot be
in a snapshot.

  create   Save the selected VMs. A stopped VM is saved directly. Running VMs
           continue to run: if a VM has guest_agent: true, TopoLab first
           freezes its file systems. Then all running VMs pause at the same
           moment, TopoLab saves them, and they continue. The pause is short.
           Output: a header and one line for each VM, with tab-separated
           columns NAME, STATE (running or stopped) and CONSISTENCY:
             frozen            the guest file systems were frozen (clean)
             crash-consistent  the same state as after a power loss
             stopped           the VM was stopped

  restore  Bring the selected VMs back to the snapshot. TopoLab stops each
           running VM with the stop --hard method, restores its disks and
           OVMF variables, and starts it again. A VM that was stopped before
           the restore stays stopped. If a disk grew after the snapshot, it
           keeps its new size. If a restore is interrupted, run the same
           command again to complete it.
           Output: a header and one line for each VM, with tab-separated
           columns NAME, BEFORE, RESULT and AFTER.

  delete   Delete the snapshot, or only the parts of the selected VMs.

  list     Show the snapshots. JSON output uses schema_version 1.

topolab up refuses a change that destroys snapshot data: a template change,
a VM removal, or a disk removal of a VM in a snapshot. Delete the snapshot,
or the part of that VM, first. topolab down deletes all snapshots.

A running exec or router exec command does not make a snapshot command wait.
`

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

type runtimeAdapter struct{ app *app.App }

func (r runtimeAdapter) Run(ctx context.Context, command RuntimeCommand) error {
	return r.app.Run(ctx, app.Command{Name: command.Name, Args: command.Args, Topology: command.Topology, WorkingDir: command.WorkingDir, Helpers: command.Helpers})
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
	qemuSystem := fs.String("qemu-system", "", "qemu-system-x86_64 executable override")
	qemuImg := fs.String("qemu-img", "", "qemu-img executable override")
	lvm := fs.String("lvm", "", "lvm executable override")
	ovmfCode := fs.String("ovmf-code", "", "OVMF code firmware override")
	ovmfVars := fs.String("ovmf-vars", "", "OVMF variables firmware override")
	xorriso := fs.String("xorriso", "", "xorriso executable override")
	fs.Usage = func() {
		fmt.Fprintln(c.Stderr, "usage: topolab [GLOBAL OPTIONS] COMMAND [OPTIONS]")
		fmt.Fprintln(c.Stderr, "       topolab exec [--user USER] [--cwd DIR] -- COMMAND...")
		fmt.Fprintln(c.Stderr, "       topolab router exec NAME [--user USER] [--cwd DIR] -- COMMAND...")
		fmt.Fprintln(c.Stderr, "       topolab stop [--hard] [NODE...]   (topolab stop --help for details)")
		fmt.Fprintln(c.Stderr, "       topolab snapshot create|restore|delete NAME [NODE...] | list   (topolab snapshot --help for details)")
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
	if name == "stop" && slices.ContainsFunc(rest[1:], func(arg string) bool { return arg == "-h" || arg == "--help" }) {
		fmt.Fprint(c.Stdout, stopHelp)
		return 0
	}
	if name == "snapshot" {
		if slices.ContainsFunc(rest[1:], func(arg string) bool { return arg == "-h" || arg == "--help" }) {
			fmt.Fprint(c.Stdout, snapshotHelp)
			return 0
		}
		if len(rest) < 2 || !snapshotSubcommands[rest[1]] {
			fmt.Fprintln(c.Stderr, "topolab: snapshot requires the create, restore, delete, or list subcommand")
			return 2
		}
		name = "snapshot-" + rest[1]
		rest = append([]string{name}, rest[2:]...)
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
