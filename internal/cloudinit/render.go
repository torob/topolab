// Package cloudinit renders deterministic NoCloud seed contents and builds
// seed ISOs through an injectable xorriso runner.
package cloudinit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Route struct{ To, Via string }
type IPv4 struct {
	DHCP       bool
	Addresses  []string
	Routes     []Route
	DNSServers []string
}
type VLAN struct {
	ID   int
	MTU  int
	IPv4 *IPv4
}
type Interface struct {
	Name, MAC string
	MTU       int
	IPv4      *IPv4
	VLANs     []VLAN
}

type Network struct {
	Interfaces []Interface
}

type UserData struct {
	Hostname       string
	AuthorizedKeys []string
}

type Seed struct{ MetaData, UserData, NetworkConfig []byte }

func Render(instanceID string, user UserData, network Network) (Seed, error) {
	if instanceID == "" || user.Hostname == "" {
		return Seed{}, errors.New("instance ID and hostname are required")
	}
	meta := []byte("instance-id: " + quote(instanceID) + "\nlocal-hostname: " + quote(user.Hostname) + "\n")
	ud, err := renderUserData(user)
	if err != nil {
		return Seed{}, err
	}
	nc, err := renderNetwork(network)
	if err != nil {
		return Seed{}, err
	}
	return Seed{MetaData: meta, UserData: ud, NetworkConfig: nc}, nil
}

func InstanceID(topology, node, storageIdentity, guestHash string) string {
	s := sha256.Sum256([]byte(topology + "\x00" + node + "\x00" + storageIdentity + "\x00" + guestHash))
	return "topolab-" + safeSlug(topology) + "-" + safeSlug(node) + "-" + hex.EncodeToString(s[:8])
}

func (s Seed) Digest() string {
	h := sha256.New()
	for _, item := range []struct {
		name string
		data []byte
	}{{"meta-data", s.MetaData}, {"user-data", s.UserData}, {"network-config", s.NetworkConfig}} {
		fmt.Fprintf(h, "%s\x00%d\x00", item.name, len(item.data))
		_, _ = h.Write(item.data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func renderUserData(u UserData) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("#cloud-config\n")
	b.WriteString("hostname: " + quote(u.Hostname) + "\n")
	b.WriteString("preserve_hostname: false\n")
	b.WriteString("ssh_deletekeys: false\n")
	b.WriteString("users:\n  - name: torob\n    lock_passwd: true\n    shell: /bin/bash\n    groups: [sudo]\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    ssh_authorized_keys:\n")
	if len(u.AuthorizedKeys) == 0 {
		b.WriteString("      []\n")
	} else {
		for _, key := range u.AuthorizedKeys {
			b.WriteString("      - " + quote(key) + "\n")
		}
	}
	return b.Bytes(), nil
}

func renderNetwork(n Network) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("version: 2\nethernets:\n")
	seen := map[string]bool{}
	for _, iface := range n.Interfaces {
		if iface.Name == "" || iface.MAC == "" {
			return nil, errors.New("network interface needs name and MAC")
		}
		if seen[iface.Name] {
			return nil, fmt.Errorf("duplicate interface %s", iface.Name)
		}
		seen[iface.Name] = true
		b.WriteString("  " + quote(iface.Name) + ":\n")
		b.WriteString("    match:\n      macaddress: " + quote(strings.ToLower(iface.MAC)) + "\n")
		b.WriteString("    set-name: " + quote(iface.Name) + "\n")
		if iface.MTU > 0 {
			fmt.Fprintf(&b, "    mtu: %d\n", iface.MTU)
		}
		var routes []Route
		if iface.IPv4 != nil {
			routes = append(routes, iface.IPv4.Routes...)
		}
		renderIPv4(&b, "    ", iface.IPv4, routes)
	}
	var vlans []struct {
		base  string
		value VLAN
	}
	for _, iface := range n.Interfaces {
		for _, vlan := range iface.VLANs {
			vlans = append(vlans, struct {
				base  string
				value VLAN
			}{iface.Name, vlan})
		}
	}
	if len(vlans) > 0 {
		b.WriteString("vlans:\n")
		for _, entry := range vlans {
			v := entry.value
			if v.ID < 1 || v.ID > 4094 {
				return nil, fmt.Errorf("invalid VLAN ID %d", v.ID)
			}
			name := fmt.Sprintf("%s.%d", entry.base, v.ID)
			if seen[name] {
				return nil, fmt.Errorf("duplicate interface %s", name)
			}
			seen[name] = true
			b.WriteString("  " + quote(name) + ":\n")
			fmt.Fprintf(&b, "    id: %d\n", v.ID)
			b.WriteString("    link: " + quote(entry.base) + "\n")
			if v.MTU > 0 {
				fmt.Fprintf(&b, "    mtu: %d\n", v.MTU)
			}
			var routes []Route
			if v.IPv4 != nil {
				routes = append(routes, v.IPv4.Routes...)
			}
			renderIPv4(&b, "    ", v.IPv4, routes)
		}
	}
	return b.Bytes(), nil
}

func renderIPv4(b *bytes.Buffer, indent string, ip *IPv4, routes []Route) {
	if ip == nil {
		return
	}
	fmt.Fprintf(b, "%sdhcp4: %t\n", indent, ip.DHCP)
	if len(ip.Addresses) > 0 {
		b.WriteString(indent + "addresses:\n")
		for _, a := range ip.Addresses {
			b.WriteString(indent + "  - " + quote(a) + "\n")
		}
	}
	if len(routes) > 0 {
		b.WriteString(indent + "routes:\n")
		for _, r := range routes {
			to := r.To
			if to == "default" {
				to = "0.0.0.0/0"
			}
			b.WriteString(indent + "  - to: " + quote(to) + "\n" + indent + "    via: " + quote(r.Via) + "\n")
		}
	}
	if len(ip.DNSServers) > 0 {
		b.WriteString(indent + "nameservers:\n" + indent + "  addresses:\n")
		for _, d := range ip.DNSServers {
			b.WriteString(indent + "    - " + quote(d) + "\n")
		}
	}
}

func quote(v string) string { b, _ := json.Marshal(v); return string(b) }
func safeSlug(v string) string {
	var b strings.Builder
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "node"
	}
	return out
}
