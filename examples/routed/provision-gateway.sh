#!/bin/sh
set -eu

usage() {
    echo "usage: $0 PRIVATE_KEY [GATEWAY_IP]" >&2
    exit 2
}

[ "$#" -ge 1 ] && [ "$#" -le 2 ] || usage
key_source=$1
gateway=${2:-192.0.2.1}
[ -f "$key_source" ] || { echo "private key not found: $key_source" >&2; exit 2; }

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
guest_script=$script_dir/guest/gateway-router.sh
run_dir=$(mktemp -d "${TMPDIR:-/tmp}/topolab-routed-provision.XXXXXX")
cleanup() { rm -rf -- "$run_dir"; }
trap cleanup EXIT HUP INT TERM

key=$run_dir/id
known_hosts=$run_dir/known_hosts
install -m 0600 "$key_source" "$key"
: > "$known_hosts"
chmod 0600 "$known_hosts"

ssh_guest() {
    ssh -i "$key" -o IdentitiesOnly=yes -o "UserKnownHostsFile=$known_hosts" \
        -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 \
        -o ServerAliveInterval=5 -o ServerAliveCountMax=3 "torob@$gateway" "$@"
}

scp_guest() {
    scp -i "$key" -o IdentitiesOnly=yes -o "UserKnownHostsFile=$known_hosts" \
        -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 "$1" "torob@$gateway:$2"
}

echo "waiting for SSH on $gateway"
deadline=$(( $(date +%s) + 300 ))
until ssh_guest true >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || { echo "timed out waiting for SSH" >&2; exit 1; }
    sleep 2
done

ssh_guest 'timeout 300s cloud-init status --wait'
scp_guest "$guest_script" /tmp/topolab-example-gateway.sh

if ! ssh_guest 'sudo sh -eu' <<'REMOTE'
install -o root -g root -m 0755 /tmp/topolab-example-gateway.sh /usr/local/sbin/.topolab-example-gateway.new
mv -f /usr/local/sbin/.topolab-example-gateway.new /usr/local/sbin/topolab-example-gateway
rm -f /tmp/topolab-example-gateway.sh
unit_tmp=$(mktemp /etc/systemd/system/.topolab-routed-example.XXXXXX)
cat > "$unit_tmp" <<'UNIT'
[Unit]
Description=TopoLab synthetic example gateway routing
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/topolab-example-gateway
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$unit_tmp"
mv -f "$unit_tmp" /etc/systemd/system/topolab-routed-example.service
systemctl daemon-reload
systemctl enable --now topolab-routed-example.service
systemctl restart topolab-routed-example.service
test "$(sysctl -n net.ipv4.ip_forward)" = 1
service=$(ip -o -4 address show | awk '$4 == "192.0.2.1/24" {print $2; exit}')
transit=$(ip -o -4 address show | awk '$4 == "198.51.100.2/30" {print $2; exit}')
test -n "$service" && test -n "$transit"
ip route get 192.0.2.10 | grep -q "dev $service"
ip route get 203.0.113.10 | grep -q "dev $transit"
iptables -w -C FORWARD -m comment --comment topolab-routed-example-owned-v1 -j TOPOLAB_EXAMPLE_FORWARD
iptables -w -C TOPOLAB_EXAMPLE_FORWARD -i "$transit" -o "$service" -s 203.0.113.0/24 -d 192.0.2.0/24 -j ACCEPT
iptables -w -C TOPOLAB_EXAMPLE_FORWARD -i "$transit" -o "$service" -j DROP
iptables -w -t nat -C POSTROUTING -m comment --comment topolab-routed-example-owned-v1 -j TOPOLAB_EXAMPLE_NAT
iptables -w -t nat -C TOPOLAB_EXAMPLE_NAT -s 192.0.2.0/24 -d 203.0.113.0/24 -o "$transit" -m comment --comment topolab-routed-example-no-nat -j RETURN
iptables -w -t nat -C TOPOLAB_EXAMPLE_NAT -s 192.0.2.0/24 ! -d 203.0.113.0/24 -o "$transit" -j MASQUERADE
REMOTE
then
    echo "gateway provisioning failed; collecting diagnostics" >&2
    ssh_guest 'sudo sh -c '\''ip -details address; ip route; sysctl net.ipv4.ip_forward; iptables-save; systemctl status --no-pager topolab-routed-example.service; journalctl --no-pager -u topolab-routed-example.service -n 200'\''' >&2 || true
    exit 1
fi

echo "synthetic gateway provisioning verified"
