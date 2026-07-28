//go:build linux

package app

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/qemu"
	"golang.org/x/sys/unix"
)

const ifNameSize = 16
const tapQueueFlags = unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_MULTI_QUEUE | unix.IFF_VNET_HDR

type ifreqFlags struct {
	Name  [ifNameSize]byte
	Flags uint16
	_     [22]byte
}

// prepareNICFiles opens persistent TAP queues for QEMU multiqueue and one
// vhost-net descriptor per accelerated queue. It records the child-side fd
// numbers in the NIC specs in the same order as exec.Cmd.ExtraFiles.
func prepareNICFiles(namespaces network.NamespaceOps, namespace string, nics []qemu.NICSpec) ([]*os.File, func(), error) {
	var files []*os.File
	cleanup := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	err := namespaces.With(namespace, func() error {
		for index := range nics {
			nic := &nics[index]
			if nic.Queues <= 1 {
				// With one queue QEMU can open both the persistent TAP and
				// /dev/vhost-net by name. Supplying vhostfds without fds is an
				// invalid QEMU netdev combination.
				nic.TAPFDs = nil
				nic.VhostFDs = nil
				continue
			} else {
				nic.TAPFDs = make([]int, 0, nic.Queues)
				for range nic.Queues {
					fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
					if err != nil {
						cleanup()
						return fmt.Errorf("open TAP %s: %w", nic.TAP, err)
					}
					var req ifreqFlags
					copy(req.Name[:], nic.TAP)
					req.Flags = tapQueueFlags
					_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&req)))
					if errno != 0 {
						_ = unix.Close(fd)
						cleanup()
						return fmt.Errorf("open TAP queue %s: %w", nic.TAP, errno)
					}
					nic.TAPFDs = append(nic.TAPFDs, 3+len(files))
					files = append(files, os.NewFile(uintptr(fd), nic.TAP))
				}
			}
			if !nic.Vhost {
				nic.VhostFDs = nil
				continue
			}
			nic.VhostFDs = make([]int, 0, nic.Queues)
			for range nic.Queues {
				fd, err := unix.Open("/dev/vhost-net", unix.O_RDWR|unix.O_CLOEXEC, 0)
				if err != nil {
					cleanup()
					return fmt.Errorf("open vhost-net for %s: %w", nic.Name, err)
				}
				nic.VhostFDs = append(nic.VhostFDs, 3+len(files))
				files = append(files, os.NewFile(uintptr(fd), "vhost-"+nic.Name))
			}
		}
		return nil
	})
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return files, cleanup, nil
}
