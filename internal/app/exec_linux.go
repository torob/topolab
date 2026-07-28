//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/state"
	"golang.org/x/sys/unix"
)

type execOptions struct {
	User, Cwd string
	Argv      []string
}

func parseExec(args []string) (execOptions, error) {
	var out execOptions
	userSet, cwdSet := false, false
	separator := slices.Index(args, "--")
	if separator < 0 {
		return out, errors.New("exec requires -- before COMMAND")
	}
	for i := 0; i < separator; i++ {
		switch args[i] {
		case "--user":
			if userSet {
				return out, errors.New("--user may be specified only once")
			}
			userSet = true
			i++
			if i >= separator || args[i] == "" {
				return out, errors.New("--user requires USER")
			}
			out.User = args[i]
		case "--cwd":
			if cwdSet {
				return out, errors.New("--cwd may be specified only once")
			}
			cwdSet = true
			i++
			if i >= separator || args[i] == "" {
				return out, errors.New("--cwd requires DIR")
			}
			out.Cwd = args[i]
		default:
			return out, fmt.Errorf("unknown exec option %q", args[i])
		}
	}
	if separator+1 >= len(args) {
		return out, errors.New("exec requires COMMAND after --")
	}
	out.Argv = append([]string(nil), args[separator+1:]...)
	return out, nil
}

type execIdentity struct {
	UID, GID uint32
	Groups   []uint32
	Name     string
	Home     string
}

type identityLookup interface {
	Lookup(string) (*user.User, error)
	LookupID(string) (*user.User, error)
	GroupIDs(*user.User) ([]string, error)
}

type osIdentityLookup struct{}

func (osIdentityLookup) Lookup(value string) (*user.User, error)     { return user.Lookup(value) }
func (osIdentityLookup) LookupID(value string) (*user.User, error)   { return user.LookupId(value) }
func (osIdentityLookup) GroupIDs(value *user.User) ([]string, error) { return value.GroupIds() }

func resolveExecIdentity(explicit string, realUID, realGID int, lookupEnv func(string) (string, bool), users identityLookup) (execIdentity, error) {
	var account *user.User
	var err error
	primaryGID := ""
	if explicit != "" {
		if _, numericErr := strconv.ParseUint(explicit, 10, 32); numericErr == nil {
			account, err = users.LookupID(explicit)
		} else {
			account, err = users.Lookup(explicit)
		}
	} else {
		sudoUID, uidOK := lookupEnv("SUDO_UID")
		sudoGID, gidOK := lookupEnv("SUDO_GID")
		if uidOK != gidOK {
			return execIdentity{}, errors.New("SUDO_UID and SUDO_GID must be supplied together")
		}
		if uidOK {
			if _, parseErr := strconv.ParseUint(sudoUID, 10, 32); parseErr != nil {
				return execIdentity{}, fmt.Errorf("invalid SUDO_UID %q", sudoUID)
			}
			if _, parseErr := strconv.ParseUint(sudoGID, 10, 32); parseErr != nil {
				return execIdentity{}, fmt.Errorf("invalid SUDO_GID %q", sudoGID)
			}
			account, err = users.LookupID(sudoUID)
			primaryGID = sudoGID
		} else {
			account, err = users.LookupID(strconv.Itoa(realUID))
			primaryGID = strconv.Itoa(realGID)
		}
	}
	if err != nil {
		return execIdentity{}, fmt.Errorf("resolve exec account: %w", err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return execIdentity{}, fmt.Errorf("account %s has invalid UID %q", account.Username, account.Uid)
	}
	if primaryGID == "" {
		primaryGID = account.Gid
	}
	gid, err := strconv.ParseUint(primaryGID, 10, 32)
	if err != nil {
		return execIdentity{}, fmt.Errorf("account %s has invalid GID %q", account.Username, primaryGID)
	}
	groupIDs, err := users.GroupIDs(account)
	if err != nil {
		return execIdentity{}, fmt.Errorf("resolve supplementary groups for %s: %w", account.Username, err)
	}
	groups := make([]uint32, 0, len(groupIDs)+1)
	seen := map[uint32]bool{}
	for _, value := range append(groupIDs, primaryGID) {
		id, parseErr := strconv.ParseUint(value, 10, 32)
		if parseErr != nil {
			return execIdentity{}, fmt.Errorf("account %s has invalid supplementary GID %q", account.Username, value)
		}
		if !seen[uint32(id)] {
			groups = append(groups, uint32(id))
			seen[uint32(id)] = true
		}
	}
	slices.Sort(groups)
	return execIdentity{UID: uint32(uid), GID: uint32(gid), Groups: groups, Name: account.Username, Home: account.HomeDir}, nil
}

type execChildConfig struct {
	Namespace string       `json:"namespace"`
	Identity  execIdentity `json:"identity"`
	Cwd       string       `json:"cwd"`
	Env       []string     `json:"env"`
}

type ChildExitError struct{ Code int }

func (e *ChildExitError) Error() string { return fmt.Sprintf("child exited with status %d", e.Code) }
func (e *ChildExitError) ExitCode() int { return e.Code }

func (a *App) runExec(ctx context.Context, layout state.Layout, manifest *state.Manifest, args []string) error {
	opts, err := parseExec(args)
	if err != nil {
		return err
	}
	if manifest == nil {
		return errors.New("topology is not up")
	}
	netState, err := manifestNetwork(manifest)
	if err != nil || netState == nil {
		return errors.New("managed network is absent; run up")
	}
	if netState.DatapathVersion != network.DatapathVersion || netState.ManagementHostNamespace == "" {
		return errors.New("topology uses a legacy datapath; run down and then up before exec")
	}
	if err := a.Network.Verify(ctx, netState); err != nil {
		return err
	}
	identity, err := resolveExecIdentity(opts.User, os.Getuid(), os.Getgid(), os.LookupEnv, osIdentityLookup{})
	if err != nil {
		return err
	}
	if opts.Cwd == "" {
		opts.Cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	lock, err := state.AcquireSharedLock(ctx, layout.LockPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Revalidate after acquiring the shared lock; a preceding writer may have
	// removed or replaced the topology while this command was waiting.
	manifest, err = state.Load(layout.ManifestPath)
	if err != nil || manifest == nil {
		return errors.New("topology is not up")
	}
	netState, err = manifestNetwork(manifest)
	if err != nil || netState == nil {
		return errors.New("managed network is absent")
	}
	if err := a.Network.Verify(ctx, netState); err != nil {
		return err
	}
	config := execChildConfig{Namespace: netState.ManagementHostNamespace, Identity: identity, Cwd: opts.Cwd, Env: sanitizedExecEnvironment(os.Environ(), identity)}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	exe, err := a.Executable()
	if err != nil {
		return err
	}
	configFD, err := unix.MemfdCreate("topolab-exec-config", unix.MFD_CLOEXEC)
	if err != nil {
		return fmt.Errorf("create exec child config: %w", err)
	}
	configFile := os.NewFile(uintptr(configFD), "topolab-exec-config")
	defer configFile.Close()
	if _, err := configFile.Write(raw); err != nil {
		return fmt.Errorf("write exec child config: %w", err)
	}
	if _, err := configFile.Seek(0, 0); err != nil {
		return fmt.Errorf("rewind exec child config: %w", err)
	}
	childArgs := []string{"__topolab-exec-child", "--"}
	childArgs = append(childArgs, opts.Argv...)
	cmd := exec.Command(exe, childArgs...)
	cmd.ExtraFiles = []*os.File{configFile}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	cmd.Env = sanitizedSupervisorEnvironment(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start exec child: %w", err)
	}
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case sig := <-signals:
			if value, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(-cmd.Process.Pid, value)
			}
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		case waitErr := <-done:
			if waitErr == nil {
				return nil
			}
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return &ChildExitError{Code: 128 + int(status.Signal())}
				}
				return &ChildExitError{Code: exitErr.ExitCode()}
			}
			return waitErr
		}
	}
}

func sanitizedSupervisorEnvironment(env []string) []string {
	return filterEnvironment(env, map[string]bool{"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "BASH_ENV": true, "ENV": true, "IFS": true, "CDPATH": true, "GLOBIGNORE": true})
}

func sanitizedExecEnvironment(env []string, identity execIdentity) []string {
	blocked := map[string]bool{"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "BASH_ENV": true, "ENV": true, "IFS": true, "CDPATH": true, "GLOBIGNORE": true, "SUDO_UID": true, "SUDO_GID": true, "SUDO_USER": true, "SUDO_COMMAND": true, "HOME": true, "USER": true, "LOGNAME": true}
	out := filterEnvironment(env, blocked)
	return append(out, "HOME="+identity.Home, "USER="+identity.Name, "LOGNAME="+identity.Name)
}

func filterEnvironment(env []string, blocked map[string]bool) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if !blocked[key] {
			out = append(out, item)
		}
	}
	return out
}

// RunExecChild permanently enters the management-host namespace, drops every
// privileged identity/capability, and execs the requested argv directly.
func RunExecChild(configFile *os.File, argv []string, stderr *os.File) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "topolab: internal exec child has no command")
		return 127
	}
	raw, err := os.ReadFile("/proc/self/fd/3")
	if configFile != nil {
		_ = configFile.Close()
	}
	if err != nil {
		fmt.Fprintf(stderr, "topolab: read exec child config: %v\n", err)
		return 127
	}
	var config execChildConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		fmt.Fprintf(stderr, "topolab: decode exec child config: %v\n", err)
		return 127
	}
	runtime.LockOSThread()
	target, err := os.Open("/var/run/netns/" + config.Namespace)
	if err == nil {
		err = unix.Setns(int(target.Fd()), unix.CLONE_NEWNET)
		_ = target.Close()
	}
	if err == nil {
		err = unix.Prctl(unix.PR_SET_KEEPCAPS, 0, 0, 0, 0)
	}
	if err == nil {
		err = unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0)
	}
	if err == nil {
		groups := make([]int, len(config.Identity.Groups))
		for i, group := range config.Identity.Groups {
			groups[i] = int(group)
		}
		err = unix.Setgroups(groups)
	}
	if err == nil {
		err = unix.Setresgid(int(config.Identity.GID), int(config.Identity.GID), int(config.Identity.GID))
	}
	if err == nil {
		err = unix.Setresuid(int(config.Identity.UID), int(config.Identity.UID), int(config.Identity.UID))
	}
	if err == nil {
		header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		var data [2]unix.CapUserData
		err = unix.Capset(&header, &data[0])
	}
	if err == nil {
		err = os.Chdir(config.Cwd)
	}
	if err != nil {
		fmt.Fprintf(stderr, "topolab: prepare exec child: %v\n", err)
		return 126
	}
	path, err := exec.LookPath(argv[0])
	if err == nil {
		err = syscall.Exec(path, argv, config.Env)
	}
	fmt.Fprintf(stderr, "topolab: exec %s: %v\n", argv[0], err)
	return 127
}
