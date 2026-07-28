//go:build linux

package app

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/torob/topolab/internal/observability"
	"golang.org/x/sys/unix"
)

type packetSource struct {
	fd, index int
	direction observability.Direction
	snaplen   int
}

func newPacketSource(device string, index int, direction observability.Direction, snaplen uint32) (observability.PacketSource, error) {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return nil, err
	}
	protocol := int(htons(unix.ETH_P_ALL))
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: uint16(protocol), Ifindex: iface.Index}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind capture to %s: %w", device, err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_AUXDATA, 1); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("enable VLAN metadata on %s: %w", device, err)
	}
	return &packetSource{fd: fd, index: index, direction: direction, snaplen: int(snaplen)}, nil
}

func (p *packetSource) ReadPacket(ctx context.Context) (observability.Packet, error) {
	buf := make([]byte, p.snaplen)
	oob := make([]byte, unix.CmsgSpace(20))
	for {
		n, oobn, _, from, err := unix.Recvmsg(p.fd, buf, oob, 0)
		if err == nil {
			if address, ok := from.(*unix.SockaddrLinklayer); ok && address.Pkttype == unix.PACKET_OUTGOING {
				continue
			}
			packet := observability.Packet{Interface: p.index, Direction: p.direction, Timestamp: time.Now().UTC(), Data: append([]byte(nil), buf[:n]...), OriginalLength: uint32(n)}
			messages, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
			if parseErr != nil {
				return observability.Packet{}, fmt.Errorf("parse packet metadata: %w", parseErr)
			}
			for _, message := range messages {
				if message.Header.Level != unix.SOL_PACKET || message.Header.Type != unix.PACKET_AUXDATA || len(message.Data) < 20 {
					continue
				}
				status := binary.NativeEndian.Uint32(message.Data[0:4])
				packet.OriginalLength = binary.NativeEndian.Uint32(message.Data[4:8])
				if status&unix.TP_STATUS_VLAN_VALID != 0 {
					tpid := uint16(0x8100)
					if status&unix.TP_STATUS_VLAN_TPID_VALID != 0 {
						tpid = binary.NativeEndian.Uint16(message.Data[18:20])
					}
					packet.VLAN = &observability.VLANTag{TPID: tpid, TCI: binary.NativeEndian.Uint16(message.Data[16:18])}
				}
			}
			return packet, nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			return observability.Packet{}, err
		}
		poll := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
		_, err = unix.Poll(poll, 100)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return observability.Packet{}, err
		}
		select {
		case <-ctx.Done():
			return observability.Packet{}, ctx.Err()
		default:
		}
	}
}
func (p *packetSource) Close() error {
	if p.fd < 0 {
		return nil
	}
	err := unix.Close(p.fd)
	p.fd = -1
	return err
}
func htons(v uint16) uint16 { return v<<8 | v>>8 }
