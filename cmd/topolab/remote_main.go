package main

import (
	"context"
	"fmt"
	"io"

	"github.com/torob/topolab/internal/remote/client"
	"github.com/torob/topolab/internal/remote/install"
)

// remoteRunner runs commands through the system ssh client.
type remoteRunner struct{}

func (remoteRunner) Run(ctx context.Context, inv client.Invocation) int {
	digest, _, err := install.SelfDigest()
	if err != nil {
		fmt.Fprintf(inv.Stderr, "topolab: hash the running executable: %v\n", err)
		return client.ExitFailure
	}
	ssh, err := client.LookSSH()
	if err != nil {
		fmt.Fprintf(inv.Stderr, "topolab: %v\n", err)
		return client.ExitFailure
	}
	r := &client.Runner{
		Dialer:      client.SSHDialer{Path: ssh, Digest: digest},
		Digest:      digest,
		DigestCache: &client.DigestCache{Path: client.DefaultDigestCachePath()},
	}
	return r.Run(ctx, inv)
}

// agentInstaller installs the running executable on a host.
type agentInstaller struct{}

func (agentInstaller) Install(ctx context.Context, destination string, sshOptions []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return install.Installer{}.Install(ctx, destination, sshOptions, stdin, stdout, stderr)
}
