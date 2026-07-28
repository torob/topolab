//go:build linux

package app

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/torob/topolab/internal/qemu"
	"golang.org/x/sys/unix"
)

type recordingNamespaceOps struct {
	withNamespace string
}

func (*recordingNamespaceOps) Run(context.Context, string, string, ...string) ([]byte, error) {
	return nil, nil
}
func (*recordingNamespaceOps) Start(string, []string, []*os.File) (int, *exec.Cmd, error) {
	return 0, nil, nil
}
func (r *recordingNamespaceOps) With(namespace string, fn func() error) error {
	r.withNamespace = namespace
	return fn()
}
func (*recordingNamespaceOps) PIDs(context.Context, string) ([]int, error) { return nil, nil }
func (*recordingNamespaceOps) SameProcess(int, string) (bool, error)       { return true, nil }

func TestPrepareNICFilesEntersFabricNamespaceEvenForSingleQueue(t *testing.T) {
	namespaces := &recordingNamespaceOps{}
	nics := []qemu.NICSpec{{Name: "eth0", TAP: "tap0", Queues: 1}}
	files, cleanup, err := prepareNICFiles(namespaces, "fabric-ns", nics)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if namespaces.withNamespace != "fabric-ns" {
		t.Fatalf("entered %q", namespaces.withNamespace)
	}
	if len(files) != 0 || len(nics[0].TAPFDs) != 0 || len(nics[0].VhostFDs) != 0 {
		t.Fatalf("single-queue descriptors changed: files=%d nic=%#v", len(files), nics[0])
	}
}

func TestMultiqueueTAPDescriptorsEnableVirtioNetworkHeaders(t *testing.T) {
	for name, flag := range map[string]uint16{
		"tap":           unix.IFF_TAP,
		"no-pi":         unix.IFF_NO_PI,
		"multiqueue":    unix.IFF_MULTI_QUEUE,
		"virtio-header": unix.IFF_VNET_HDR,
	} {
		if tapQueueFlags&flag == 0 {
			t.Errorf("TAP queue flags omit %s", name)
		}
	}
}
