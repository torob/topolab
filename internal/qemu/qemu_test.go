package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

type identityProcess struct{ args []string }

type runnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func (p identityProcess) Alive(int) bool                    { return true }
func (p identityProcess) CommandLine(int) ([]string, error) { return p.args, nil }
func (p identityProcess) Signal(int, os.Signal) error       { return nil }

func TestBuildArgs(t *testing.T) {
	s := NodeSpec{Topology: "lab", Name: "vm", CPU: CPU{Model: "host", Sockets: 1, Cores: 2, Threads: 1}, Memory: Memory{MiB: 2048, Balloon: true}, Firmware: Firmware{Code: "/fw/code", Vars: "/fw/vars"}, QMPPath: "/run/qmp", PIDFile: "/run/pid", SerialSocket: "/run/serial", SeedPath: "/seed.iso", Disks: []DiskSpec{{Name: "root", Path: "/root.qcow2", Bus: "scsi"}, {Name: "data", Path: "/data.qcow2", Bus: "nvme"}}, NICs: []NICSpec{{Name: "eth0", Model: "virtio", MAC: "02:00:00:00:00:01", TAP: "tap0", Queues: 2, TAPFDs: []int{3, 4}, Vhost: true, VhostFDs: []int{5, 6}}}, Boot: BootSpec{Order: []string{"disk", "network"}}}
	args, err := NewBuilder("").Build(s)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"q35,accel=kvm", "cpus=2,sockets=1,cores=2,threads=1", "virtio-scsi-pci", "nvme,drive=", "serial=topolab-", "fds=3:4", "vhostfds=5:6", "mq=on,vectors=6", "bootindex=1001", "virtio-balloon-pci", "id=seed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

func TestImageInfoAllowsReadOnlyInspectionOfLiveDisk(t *testing.T) {
	manager := NewImageManager(runnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "qemu-img" || strings.Join(args, " ") != "info --force-share --output=json /live.qcow2" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return []byte(`{"format":"qcow2","virtual-size":21474836480}`), nil
	}), "")
	if _, err := manager.Info(context.Background(), "/live.qcow2"); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRejectsNonVirtioMultiqueue(t *testing.T) {
	s := NodeSpec{Topology: "l", Name: "n", CPU: CPU{Model: "host", Sockets: 1, Cores: 1, Threads: 1}, Memory: Memory{MiB: 128}, Firmware: Firmware{Code: "c", Vars: "v"}, QMPPath: "q", PIDFile: "p", SerialSocket: "s", NICs: []NICSpec{{Name: "n", Model: "e1000", MAC: "02:00:00:00:00:01", TAP: "tap", Queues: 2, TAPFDs: []int{3, 4}}}}
	if _, err := NewBuilder("").Build(s); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildRejectsVhostFDsWithoutTAPFDs(t *testing.T) {
	s := NodeSpec{Topology: "l", Name: "n", CPU: CPU{Model: "host", Sockets: 1, Cores: 1, Threads: 1}, Memory: Memory{MiB: 128}, Firmware: Firmware{Code: "c", Vars: "v"}, QMPPath: "q", PIDFile: "p", SerialSocket: "s", NICs: []NICSpec{{Name: "n", Model: "virtio", MAC: "02:00:00:00:00:01", TAP: "tap", Queues: 1, Vhost: true, VhostFDs: []int{3}}}}
	if _, err := NewBuilder("").Build(s); err == nil || !strings.Contains(err.Error(), "vhost FDs require TAP FDs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildRejectsOverlongDiskSerial(t *testing.T) {
	s := NodeSpec{Topology: "l", Name: "n", CPU: CPU{Model: "host", Sockets: 1, Cores: 1, Threads: 1}, Memory: Memory{MiB: 128}, Firmware: Firmware{Code: "c", Vars: "v"}, QMPPath: "q", PIDFile: "p", SerialSocket: "s", Disks: []DiskSpec{{Name: "root", Path: "/disk", Bus: "scsi", Serial: "123456789012345678901"}}}
	if _, err := NewBuilder("").Build(s); err == nil || !strings.Contains(err.Error(), "20-character limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQMPHandshakeAndCommands(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		enc := json.NewEncoder(server)
		dec := json.NewDecoder(bufio.NewReader(server))
		if err := enc.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}}}); err != nil {
			done <- err
			return
		}
		for i := 0; i < 3; i++ {
			var req map[string]any
			if err := dec.Decode(&req); err != nil {
				done <- err
				return
			}
			id := req["id"]
			if req["execute"] == "query-balloon" {
				_ = enc.Encode(map[string]any{"event": "RESET"})
				_ = enc.Encode(map[string]any{"return": map[string]any{"actual": uint64(1024)}, "id": id})
			} else {
				_ = enc.Encode(map[string]any{"return": map[string]any{}, "id": id})
			}
		}
		done <- nil
	}()
	q := NewQMPClient("unused", func(context.Context, string, string) (net.Conn, error) { return client, nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := q.Balloon(ctx, 2048); err != nil {
		t.Fatal(err)
	}
	actual, err := q.QueryBalloon(ctx)
	if err != nil || actual != 1024 {
		t.Fatalf("actual=%d err=%v", actual, err)
	}
	_ = q.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGiBOverflow(t *testing.T) {
	if got, err := GiBToBytes(2); err != nil || got != 2<<30 {
		t.Fatalf("got %d %v", got, err)
	}
	if _, err := GiBToBytes(^uint64(0)); err == nil {
		t.Fatal("expected overflow")
	}
}

func TestProcessIdentityDoesNotAcceptNodeNamePrefix(t *testing.T) {
	p := identityProcess{args: []string{"qemu-system-x86_64", "-name", "guest=topolab:lab:vm2,debug-threads=on"}}
	if err := VerifyProcessIdentity(p, 42, "topolab:lab:vm"); err == nil {
		t.Fatal("node vm must not adopt vm2")
	}
	if err := VerifyProcessIdentity(p, 42, "topolab:lab:vm2"); err != nil {
		t.Fatal(err)
	}
}

func TestPCIAllocatorReservesQ35SouthbridgeSlot(t *testing.T) {
	allocator := newPCIAllocator()
	for i := 0; i < 26; i++ { // slots 0x5 through 0x1e
		if _, err := allocator.Next(); err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
	}
	if _, err := allocator.Next(); err == nil {
		t.Fatal("allocator used reserved q35 slot 0x1f")
	}
}
