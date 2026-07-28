#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "routed example acceptance requires effective UID 0" >&2
    exit 1
fi

repo_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
topolab=${TOPOLAB_BINARY:-$repo_dir/bin/topolab}
template=${TOPOLAB_TEMPLATE:-$repo_dir/hack/template/ubuntu-26.04-topolab.qcow2}
working_dir=${1:-/var/tmp/topolab-routed-acceptance}
topology=$repo_dir/examples/routed/topology.yml

[ -x "$topolab" ] || { echo "build $topolab first" >&2; exit 1; }
[ -f "$template" ] || { echo "template not found: $template" >&2; exit 1; }
[ ! -e "$working_dir" ] || { echo "refusing non-empty acceptance path: $working_dir" >&2; exit 1; }

install -d -m 0700 "$working_dir"
key=/tmp/topolab-routed-acceptance-key-$$
ssh-keygen -q -t ed25519 -N '' -f "$key"
chown "${SUDO_UID:-0}:${SUDO_GID:-0}" "$key" "$key.pub"
capture_pid=
passed=false
cleanup() {
    if [ -n "$capture_pid" ] && kill -0 "$capture_pid" 2>/dev/null; then
        kill -INT "$capture_pid" 2>/dev/null || true
        wait "$capture_pid" 2>/dev/null || true
    fi
    rm -f -- "$key" "$key.pub"
    if [ "$passed" != true ]; then
        echo "acceptance failed; topology state and diagnostics remain in $working_dir" >&2
    fi
}
trap cleanup EXIT HUP INT TERM
export ROUTED_SSH_PUBLIC_KEY
ROUTED_SSH_PUBLIC_KEY=$(sed -n '1p' "$key.pub")
export TOPOLAB_TEMPLATE="$template"

run() {
    "$topolab" --working-dir "$working_dir" --file "$topology" "$@"
}

snapshot_root() {
	ip -j -d link show | jq 'map(del(.linkinfo.info_data.gc_timer, .linkinfo.info_data.hello_timer, .linkinfo.info_data.tcn_timer, .linkinfo.info_data.topology_change_timer))'
	ip -j -4 route show table all
	sysctl -n net.ipv4.ip_forward
	nft --stateless list ruleset
	iptables-save | sed -E '/^# (Generated|Completed)/d; s/ \[[0-9]+:[0-9]+\]$/ [0:0]/'
}

controller() {
	run exec -- "$@"
}

gateway_nat_packets() {
    controller ssh -i "$key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
        -o BatchMode=yes -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 torob@192.0.2.1 \
        "sudo iptables -w -t nat -nvxL TOPOLAB_EXAMPLE_NAT | awk '\$3 == \"MASQUERADE\" { total += \$1 } END { print total + 0 }'"
}

edge_nat_packets() {
    python3 - "$working_dir" <<'PY'
import json, re, subprocess, sys
manifest = sys.argv[1] + "/.topolab/topologies/routed/manifest.json"
with open(manifest, encoding="utf-8") as f:
    table = json.load(f)["network_state"]["root_nft_table"]
rules = subprocess.check_output(
    ["nft", "list", "chain", "inet", table, "postrouting"], text=True
)
total = 0
for line in rules.splitlines():
    if "external-nat" not in line:
        continue
    match = re.search(r"counter packets ([0-9]+)", line)
    if match:
        total += int(match.group(1))
print(total)
PY
}

wait_gateway_service() {
    deadline=$(( $(date +%s) + 180 ))
    until controller ssh -i "$key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
        -o BatchMode=yes -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 torob@192.0.2.1 \
        'systemctl is-active --quiet topolab-routed-example.service && test "$(sysctl -n net.ipv4.ip_forward)" = 1 && sudo iptables -w -C FORWARD -m comment --comment topolab-routed-example-owned-v1 -j TOPOLAB_EXAMPLE_FORWARD && sudo iptables -w -t nat -C POSTROUTING -m comment --comment topolab-routed-example-owned-v1 -j TOPOLAB_EXAMPLE_NAT' >/dev/null 2>&1; do
        [ "$(date +%s)" -lt "$deadline" ] || { echo "gateway service did not recover" >&2; exit 1; }
        sleep 2
    done
}

snapshot_root > "$working_dir/root-before.txt"
run config > "$working_dir/normalized.yml"
run up
manifest=$working_dir/.topolab/topologies/routed/manifest.json
fabric=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["network_state"]["fabric_namespace"])' "$manifest")
management=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["network_state"]["management_host_namespace"])' "$manifest")
qemu_pid=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["nodes"]["gateway"]["pid"])' "$manifest")
controller sh -c 'readlink /proc/self/ns/net; sh -c '\''readlink /proc/self/ns/net; sh -c "readlink /proc/self/ns/net"'\''' > "$working_dir/exec-namespace-inodes"
[ "$(sort -u "$working_dir/exec-namespace-inodes" | wc -l)" -eq 1 ]
[ "$(sed -n '1p' "$working_dir/exec-namespace-inodes")" = "$(readlink /proc/self/ns/net)" ] && { echo "exec remained in root namespace" >&2; exit 1; }
[ "$(readlink "/proc/$qemu_pid/ns/net")" = "$(ip netns exec "$fabric" readlink /proc/self/ns/net)" ]
[ "$(readlink "/proc/$qemu_pid/ns/net")" != "$(ip netns exec "$management" readlink /proc/self/ns/net)" ]
controller "$repo_dir/examples/routed/provision-gateway.sh" "$key"
controller "$repo_dir/examples/routed/provision-gateway.sh" "$key"
controller ssh -i "$key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
    -o BatchMode=yes -o UserKnownHostsFile=/dev/null torob@192.0.2.10 true
export ROUTED_SSH_PRIVATE_KEY=$key
controller ansible-playbook \
    -i "$repo_dir/examples/routed/ansible/inventory.yml" \
    "$repo_dir/examples/routed/ansible/reachability.yml"
unset ROUTED_SSH_PRIVATE_KEY

run counters --format json > "$working_dir/counters-before.json"
run capture gateway-transit --endpoint gateway:transit --direction both \
    --duration 20s --output captures/routed/acceptance-client-to-service.pcapng &
capture_pid=$!

"$repo_dir/examples/routed/verify.sh" "$key" routed
wait "$capture_pid"
capture_pid=
gateway_nat_routed=$(gateway_nat_packets)
edge_nat_routed=$(edge_nat_packets)
run capture gateway-transit --endpoint gateway:transit --direction both \
    --duration 45s --output captures/routed/acceptance-internet.pcapng &
capture_pid=$!
"$repo_dir/examples/routed/verify.sh" "$key" service-internet
gateway_nat_after=$(gateway_nat_packets)
edge_nat_after=$(edge_nat_packets)
[ "$gateway_nat_after" -gt "$gateway_nat_routed" ] || { echo "gateway NAT did not count service-network Internet traffic" >&2; exit 1; }
[ "$edge_nat_after" -gt "$edge_nat_routed" ] || { echo "edge NAT did not count service-network Internet traffic" >&2; exit 1; }
"$repo_dir/examples/routed/verify.sh" "$key" internet
wait "$capture_pid"
capture_pid=
run counters --format json > "$working_dir/counters-after.json"

python3 - "$working_dir/counters-before.json" "$working_dir/counters-after.json" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    before = json.load(f)
with open(sys.argv[2], encoding="utf-8") as f:
    after = json.load(f)
b = {link["name"]: link for link in before["links"]}["gateway-transit"]
a = {link["name"]: link for link in after["links"]}["gateway-transit"]
if b["generation_id"] != a["generation_id"]:
    raise SystemExit("gateway-transit counter generation changed during traffic test")
if len(b["endpoints"]) != 2 or len(a["endpoints"]) != 2:
    raise SystemExit("gateway-transit did not report exactly two endpoints")
for old_ep, new_ep in zip(b["endpoints"], a["endpoints"], strict=True):
    deltas = []
    for direction in ("ingress", "egress"):
        x, y = old_ep[direction].get("packets"), new_ep[direction].get("packets")
        if x is not None and y is not None:
            deltas.append(y - x)
    if not any(delta > 0 for delta in deltas):
        raise SystemExit(f"no packet delta for gateway-transit endpoint {new_ep['node']}:{new_ep['interface']}")
PY

python3 - "$working_dir/captures/routed/acceptance-client-to-service.pcapng" <<'PY'
import ipaddress, struct, sys
data = open(sys.argv[1], "rb").read()
offset = epb = untranslated = translated = 0
client = ipaddress.IPv4Address("203.0.113.10").packed
applications = {ipaddress.IPv4Address("192.0.2.10").packed}
gateway = ipaddress.IPv4Address("198.51.100.2").packed
while offset + 12 <= len(data):
    kind, size = struct.unpack_from("<II", data, offset)
    if size < 12 or offset + size > len(data):
        raise SystemExit("invalid pcapng block framing")
    if kind == 6:
        epb += 1
        captured = struct.unpack_from("<I", data, offset + 20)[0]
        frame = data[offset + 28:offset + 28 + captured]
        if len(frame) >= 34:
            ethertype = struct.unpack_from("!H", frame, 12)[0]
            ipoff = 14
            if ethertype in (0x8100, 0x88A8) and len(frame) >= 38:
                ethertype = struct.unpack_from("!H", frame, 16)[0]
                ipoff = 18
            if ethertype == 0x0800 and len(frame) >= ipoff + 20 and frame[ipoff + 9] == 6:
                ihl = (frame[ipoff] & 0x0f) * 4
                if ihl >= 20 and len(frame) >= ipoff + ihl + 4:
                    source, destination = frame[ipoff + 12:ipoff + 16], frame[ipoff + 16:ipoff + 20]
                    source_port = struct.unpack_from("!H", frame, ipoff + ihl)[0]
                    if destination == client and source_port == 22:
                        untranslated += source in applications
                        translated += source == gateway
    offset += size
if offset != len(data) or epb == 0:
    raise SystemExit("capture contains no enhanced packet blocks")
if untranslated == 0:
    raise SystemExit("capture contains no untranslated application SSH replies")
if translated:
    raise SystemExit("capture contains gateway-translated application SSH replies")
PY

run restart gateway
wait_gateway_service
"$repo_dir/examples/routed/verify.sh" "$key"
run stop gateway
run start gateway
wait_gateway_service
"$repo_dir/examples/routed/verify.sh" "$key"

# A recoverable move forces reconciliation to create a fresh gateway root overlay.
run stop gateway
old_overlay=$working_dir/gateway-root-before-recreate.qcow2
python3 - "$working_dir" "$old_overlay" <<'PY'
import json, os, sys
working, destination = map(os.path.realpath, sys.argv[1:])
manifest = os.path.join(working, ".topolab", "topologies", "routed", "manifest.json")
with open(manifest, encoding="utf-8") as f:
    root = os.path.realpath(json.load(f)["nodes"]["gateway"]["root_disk"]["path"])
if os.path.commonpath((working, root)) != working:
    raise SystemExit(f"refusing overlay outside working directory: {root}")
os.rename(root, destination)
PY
run up
controller "$repo_dir/examples/routed/provision-gateway.sh" "$key"
"$repo_dir/examples/routed/verify.sh" "$key"

routers=$(python3 -c 'import json,sys; print(" ".join(json.load(open(sys.argv[1]))["network_state"]["namespaces"].values()))' "$working_dir/.topolab/topologies/routed/manifest.json")
run down
test ! -e "$working_dir/.topolab/topologies/routed/manifest.json"
test -s "$working_dir/captures/routed/acceptance-internet.pcapng"
for namespace in "$fabric" "$management" $routers; do
	if ip netns list | awk '{print $1}' | grep -Fxq "$namespace"; then
		echo "namespace remains after down: $namespace" >&2
		exit 1
	fi
done
snapshot_root > "$working_dir/root-after.txt"
cmp "$working_dir/root-before.txt" "$working_dir/root-after.txt"

rm -f -- "$old_overlay"
passed=true
cleanup
trap - EXIT HUP INT TERM
echo "routed example acceptance passed; captures retained beneath $working_dir/captures/routed"
