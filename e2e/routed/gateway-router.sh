#!/bin/sh
set -eu

interface_for() {
    preferred=$1
    address=$2
    if ip link show dev "$preferred" >/dev/null 2>&1; then
        printf '%s\n' "$preferred"
        return
    fi
    ip -o -4 address show | awk -v address="$address" '$4 == address {print $2; found=1; exit} END {if (!found) exit 1}'
}

SERVICE_INTERFACE=${SERVICE_INTERFACE:-$(interface_for service 192.0.2.1/24)}
TRANSIT_INTERFACE=${TRANSIT_INTERFACE:-$(interface_for transit 198.51.100.2/30)}
SERVICE_CIDR=192.0.2.0/24
CLIENT_CIDR=203.0.113.0/24
FORWARD_CHAIN=TOPOLAB_EXAMPLE_FORWARD
NAT_CHAIN=TOPOLAB_EXAMPLE_NAT
OWNER_COMMENT=topolab-routed-example-owned-v1

install -d -m 0755 /etc/sysctl.d
printf '%s\n' 'net.ipv4.ip_forward=1' > /etc/sysctl.d/90-topolab-routed-example.conf
sysctl -q -p /etc/sysctl.d/90-topolab-routed-example.conf

ensure_chain() {
    table=$1
    chain=$2
    if iptables -w -t "$table" -L "$chain" -n >/dev/null 2>&1; then
        iptables -w -t "$table" -C "$chain" -m comment --comment "$OWNER_COMMENT" 2>/dev/null || {
            echo "refusing to adopt foreign iptables chain $table/$chain" >&2
            exit 1
        }
    else
        iptables -w -t "$table" -N "$chain"
    fi
    iptables -w -t "$table" -F "$chain"
    iptables -w -t "$table" -A "$chain" -m comment --comment "$OWNER_COMMENT"
}

ensure_jump() {
    table=$1
    parent=$2
    chain=$3
    while iptables -w -t "$table" -C "$parent" -m comment --comment "$OWNER_COMMENT" -j "$chain" 2>/dev/null; do
        iptables -w -t "$table" -D "$parent" -m comment --comment "$OWNER_COMMENT" -j "$chain"
    done
    iptables -w -t "$table" -I "$parent" 1 -m comment --comment "$OWNER_COMMENT" -j "$chain"
}

ensure_chain filter "$FORWARD_CHAIN"
ensure_jump filter FORWARD "$FORWARD_CHAIN"
iptables -w -A "$FORWARD_CHAIN" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -w -A "$FORWARD_CHAIN" -i "$SERVICE_INTERFACE" -o "$TRANSIT_INTERFACE" -s "$SERVICE_CIDR" -j ACCEPT
iptables -w -A "$FORWARD_CHAIN" -i "$TRANSIT_INTERFACE" -o "$SERVICE_INTERFACE" -s "$CLIENT_CIDR" -d "$SERVICE_CIDR" -j ACCEPT
iptables -w -A "$FORWARD_CHAIN" -i "$TRANSIT_INTERFACE" -o "$SERVICE_INTERFACE" -j DROP

ensure_chain nat "$NAT_CHAIN"
ensure_jump nat POSTROUTING "$NAT_CHAIN"
iptables -w -t nat -A "$NAT_CHAIN" -s "$SERVICE_CIDR" -d "$CLIENT_CIDR" -o "$TRANSIT_INTERFACE" -m comment --comment topolab-routed-example-no-nat -j RETURN
iptables -w -t nat -A "$NAT_CHAIN" -s "$SERVICE_CIDR" ! -d "$CLIENT_CIDR" -o "$TRANSIT_INTERFACE" -j MASQUERADE
