#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "network integration requires effective UID 0" >&2
    exit 1
fi

repo_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
topolab=${TOPOLAB_BINARY:-$repo_dir/bin/topolab}
[ -x "$topolab" ] || { echo "build $topolab first" >&2; exit 1; }

test_dir=$(mktemp -d "${TMPDIR:-/tmp}/topolab-network-integration.XXXXXX")
topology=$test_dir/topology.yml
topology2=$test_dir/topology-two.yml
legacy_topology=$test_dir/legacy-topology.yml
working=$test_dir/work
original_forward=$(sysctl -n net.ipv4.ip_forward)
sentinel_chain=TLAB_INT_$$
sentinel_table=tlab_int_$$
legacy_device=tllegacy$$
capture_pid=
foreign_pid=
lock_marker=
passed=false
remove_sentinels() {
	iptables -w -D OUTPUT -m comment --comment topolab-integration-sentinel -j "$sentinel_chain" >/dev/null 2>&1 || true
	iptables -w -F "$sentinel_chain" >/dev/null 2>&1 || true
	iptables -w -X "$sentinel_chain" >/dev/null 2>&1 || true
	nft delete table inet "$sentinel_table" >/dev/null 2>&1 || true
}
cleanup() {
    if [ -n "$capture_pid" ] && kill -0 "$capture_pid" 2>/dev/null; then
        kill -INT "$capture_pid" 2>/dev/null || true
        wait "$capture_pid" 2>/dev/null || true
    fi
	if [ -n "$foreign_pid" ] && kill -0 "$foreign_pid" 2>/dev/null; then
		kill "$foreign_pid" 2>/dev/null || true
		wait "$foreign_pid" 2>/dev/null || true
	fi
	[ -z "$lock_marker" ] || rm -f -- "$lock_marker"
    "$topolab" --file "$topology" --working-dir "$working" down >/dev/null 2>&1 || true
	[ ! -e "$topology2" ] || "$topolab" --file "$topology2" --working-dir "$working" down >/dev/null 2>&1 || true
	[ ! -e "$legacy_topology" ] || "$topolab" --file "$legacy_topology" --working-dir "$working" down >/dev/null 2>&1 || true
	ip link del dev "$legacy_device" >/dev/null 2>&1 || true
	remove_sentinels
	if [ "$passed" = true ]; then
		rm -rf -- "$test_dir"
	else
		echo "integration failed; diagnostics retained in $test_dir" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

write_topology() {
    loss=$1
    cat > "$topology" <<EOF
name: integration
management_host:
  interfaces:
    management:
      mtu: 1500
      ipv4: {addresses: [192.0.2.254/24]}
routers:
  left:
    interfaces:
      trunk:
        mtu: 1500
        vlans:
          10:
            mtu: 1500
            ipv4: {addresses: [192.0.2.1/24]}
  right:
    interfaces:
      access:
        mtu: 1500
        ipv4: {addresses: [192.0.2.2/24]}
switches:
  lan: {mtu: 1500, vlan_filtering: true}
links:
  left-lan:
    mtu: 1400
    endpoints:
      - node: left
        interface: trunk
        egress: {loss_percent: $loss}
      - node: lan
        interface: left
        port: {mode: trunk, allowed_vlans: [10]}
  right-lan:
    endpoints:
      - {node: right, interface: access}
      - {node: lan, interface: right, port: {mode: access, vlan: 10}}
  management-lan:
    endpoints:
      - {node: management_host, interface: management}
      - {node: lan, interface: management-host, port: {mode: access, vlan: 10}}
EOF
}

run() { "$topolab" --file "$topology" --working-dir "$working" "$@"; }
run2() { "$topolab" --file "$topology2" --working-dir "$working" "$@"; }

snapshot_root() {
	ip -j -d link show | jq 'map(del(.linkinfo.info_data.gc_timer, .linkinfo.info_data.hello_timer, .linkinfo.info_data.tcn_timer, .linkinfo.info_data.topology_change_timer))'
	ip -j -4 route show table all
	nft --stateless list ruleset
	iptables-save | sed -E '/^# (Generated|Completed)/d; s/ \[[0-9]+:[0-9]+\]$/ [0:0]/'
}

write_topology 100
run config > "$test_dir/normalized.yml"
iptables -w -N "$sentinel_chain"
iptables -w -I OUTPUT 1 -m comment --comment topolab-integration-sentinel -j "$sentinel_chain"
nft add table inet "$sentinel_table"
nft add chain inet "$sentinel_table" keep
nft add rule inet "$sentinel_table" keep counter comment topolab-integration-sentinel
snapshot_root > "$test_dir/root-before.json"
run up
python3 - "$working/.topolab/topologies/integration/manifest.json" <<'PY'
import json, os, sys
with open(sys.argv[1], encoding="utf-8") as f:
    manifest = json.load(f)
network = manifest["network_state"]
if manifest.get("nodes"):
    raise SystemExit("router-only topology unexpectedly created QEMU nodes")
if not all(name in network["namespaces"] for name in ("left", "right")):
    raise SystemExit("router namespaces were not created")
if network.get("taps"):
    raise SystemExit("router-only topology unexpectedly created TAPs")
nodes_dir = os.path.join(os.path.dirname(sys.argv[1]), "nodes")
for root, _, files in os.walk(nodes_dir):
    for name in files:
        if name.endswith((".qcow2", ".iso")):
            raise SystemExit(f"router-only topology created QEMU material: {os.path.join(root, name)}")
PY
snapshot_root > "$test_dir/root-during.json"
cmp "$test_dir/root-before.json" "$test_dir/root-during.json"
sed 's/^name: integration$/name: dupe/' "$topology" > "$topology2"
run2 up
run2 exec -- ping -c 1 -W 2 192.0.2.2 >/dev/null
snapshot_root > "$test_dir/root-during-two.json"
cmp "$test_dir/root-before.json" "$test_dir/root-during-two.json"
run2 down
expected_uid=${SUDO_UID:-0}
expected_gid=${SUDO_GID:-0}
identity=$(run exec -- sh -c 'id -u; id -g; id -G; awk '\''/^Cap(Inh|Prm|Eff|Amb):/ {print $2}'\'' /proc/self/status')
[ "$(printf '%s\n' "$identity" | sed -n '1p')" = "$expected_uid" ]
[ "$(printf '%s\n' "$identity" | sed -n '2p')" = "$expected_gid" ]
[ "$(printf '%s\n' "$identity" | sed -n '4,$p' | sort -u)" = "0000000000000000" ]
[ "$(printf 'topolab-stdio' | run exec -- cat)" = "topolab-stdio" ]
script -qec "$topolab --file $topology --working-dir $working exec -- sh -c 'test -t 0 && test -t 1 && test -t 2'" /dev/null >/dev/null
[ "$(run exec --cwd /tmp -- sh -c 'pwd; printf %s "$HOME"' | sed -n '1p')" = /tmp ]
if run exec -- sh -c 'exit 42'; then
	echo "exec lost child exit status" >&2
	exit 1
else
	[ "$?" -eq 42 ]
fi
signal_marker=/tmp/topolab-exec-signal-$$
rm -f -- "$signal_marker"
"$topolab" --file "$topology" --working-dir "$working" exec -- \
	sh -c 'trap '\''exit 23'\'' TERM; : > "$1"; while :; do sleep 1; done' sh "$signal_marker" &
signal_supervisor=$!
deadline=$(( $(date +%s) + 5 ))
while [ ! -e "$signal_marker" ]; do
	[ "$(date +%s)" -lt "$deadline" ] || { echo "exec signal child did not start" >&2; exit 1; }
	sleep 0.05
done
kill -TERM "$signal_supervisor"
if wait "$signal_supervisor"; then signal_status=0; else signal_status=$?; fi
rm -f -- "$signal_marker"
[ "$signal_status" -eq 23 ]
run ps > "$test_dir/ps.txt"
grep -q 'left.*router' "$test_dir/ps.txt"
grep -q 'lan.*switch' "$test_dir/ps.txt"
grep -q 'management_host:management.*isolation=true' "$test_dir/ps.txt"
grep -q 'fabric-namespace.*ready' "$test_dir/ps.txt"
grep -q 'management-host-namespace.*ready' "$test_dir/ps.txt"

run counters --format json left-lan > "$test_dir/before-loss.json"
run capture left-lan --endpoint left:trunk --direction egress \
    --duration 3s --output captures/integration/loss.pcapng >/dev/null &
capture_pid=$!
sleep 1
if run exec -- ping -c 2 -W 1 192.0.2.1 >/dev/null 2>&1; then
    echo "100-percent egress loss unexpectedly delivered replies" >&2
    exit 1
fi
wait "$capture_pid"
capture_pid=
run counters --format json left-lan > "$test_dir/after-loss.json"

python3 - "$test_dir/before-loss.json" "$test_dir/after-loss.json" "$working/captures/integration/loss.pcapng" <<'PY'
import json, struct, sys
before, after = (json.load(open(path, encoding="utf-8")) for path in sys.argv[1:3])
b, a = before["links"][0], after["links"][0]
if b["generation_id"] != a["generation_id"]:
    raise SystemExit("generation changed during loss test")
old, new = next((old, new) for old, new in zip(b["endpoints"], a["endpoints"], strict=True) if new["node"] == "left")
if new["impairment"]["offered_packets"] <= old["impairment"]["offered_packets"]:
    raise SystemExit("offered counter did not increase")
if new["impairment"]["configured_loss_drops"] <= old["impairment"]["configured_loss_drops"]:
    raise SystemExit("configured-loss counter did not increase")
data = open(sys.argv[3], "rb").read()
offset = packets = 0
while offset + 12 <= len(data):
    kind, size = struct.unpack_from("<II", data, offset)
    if size < 12 or offset + size > len(data): raise SystemExit("invalid pcapng")
    packets += kind == 6
    offset += size
if packets: raise SystemExit("post-impairment loss capture contains delivered frames")
PY

old_generation=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["links"][0]["generation_id"])' "$test_dir/after-loss.json")
write_topology 0
run up
run exec -- ping -c 2 -W 2 192.0.2.1 >/dev/null
run exec -- ping -c 2 -W 2 192.0.2.2 >/dev/null
run counters --format json left-lan > "$test_dir/repaired.json"
new_generation=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["links"][0]["generation_id"])' "$test_dir/repaired.json")
[ "$new_generation" != "$old_generation" ]

# Missing resources are repaired; foreign ownership is rejected before down.
manifest=$working/.topolab/topologies/integration/manifest.json
bridge=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["network_state"]["links"]["left-lan"]["bridge"])' "$manifest")
fabric=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["network_state"]["fabric_namespace"])' "$manifest")
management=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["network_state"]["management_host_namespace"])' "$manifest")
ip netns exec "$fabric" ip link del dev "$bridge"
run up
python3 - "$manifest" > "$test_dir/bridge-owner" <<'PY'
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))["network_state"]
bridge = state["links"]["left-lan"]["bridge"]
resource = next(r for r in state["resources"] if r["name"] == bridge)
print(bridge, resource["alias"])
PY
read -r bridge owner < "$test_dir/bridge-owner"
ip netns exec "$fabric" ip link set dev "$bridge" alias foreign-owner
if run down >/dev/null 2>&1; then
    echo "down accepted a foreign ownership marker" >&2
    exit 1
fi
ip netns exec "$fabric" ip link set dev "$bridge" alias "$owner"

# A missing topology namespace is rebuilt, while a present namespace with a
# foreign ownership marker is rejected without mutation.
ip netns del "$management"
run up
fabric=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["network_state"]["fabric_namespace"])' "$manifest")
management=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["network_state"]["management_host_namespace"])' "$manifest")
fabric_owner=$(python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["network_state"]; print(s["namespace_aliases"]["fabric"])' "$manifest")
ip -n "$fabric" link set dev lo alias foreign-owner
if run up >/dev/null 2>&1; then
	echo "up accepted a fabric namespace with a foreign ownership marker" >&2
	exit 1
fi
ip -n "$fabric" link set dev lo alias "$fabric_owner"

ip netns exec "$management" sleep 60 &
foreign_pid=$!
if run down >/dev/null 2>&1; then
	echo "down accepted a foreign namespace process" >&2
	exit 1
fi
kill "$foreign_pid"
wait "$foreign_pid" 2>/dev/null || true
foreign_pid=

start=$(date +%s)
lock_marker=/tmp/topolab-exec-lock-$$
rm -f -- "$lock_marker"
run exec -- sh -c 'touch "$1"; sleep 2' sh "$lock_marker" &
exec_pid=$!
deadline=$(( $(date +%s) + 5 ))
while [ ! -e "$lock_marker" ]; do
	[ "$(date +%s)" -lt "$deadline" ] || { echo "exec did not acquire shared lock" >&2; exit 1; }
	sleep 0.05
done
run down
wait "$exec_pid"
rm -f -- "$lock_marker"
[ $(( $(date +%s) - start )) -ge 2 ]

# A v1 root-datapath manifest is never silently migrated, but its exact owned
# resources remain removable by the new down implementation.
cat > "$legacy_topology" <<'EOF'
name: legacy
management_host:
  interfaces:
    management:
      ipv4: {addresses: [10.88.0.2/30]}
routers:
  router:
    interfaces:
      management:
        ipv4: {addresses: [10.88.0.1/30]}
links:
  management:
    endpoints:
      - {node: management_host, interface: management}
      - {node: router, interface: management}
EOF
legacy_dir=$working/.topolab/topologies/legacy
install -d -m 0700 "$legacy_dir"
ip link add dev "$legacy_device" type dummy
ip link set dev "$legacy_device" alias topolab:v1:legacy:legacy-dummy
cat > "$legacy_dir/manifest.json" <<EOF
{
  "version": 1,
  "topology": "legacy",
  "nodes": {},
  "links": {},
  "host_interfaces": {},
  "network_state": {
    "topology": "legacy",
    "resources": [
      {"kind": "link", "name": "$legacy_device", "alias": "topolab:v1:legacy:legacy-dummy"}
    ],
    "links": {},
    "fabric_namespace": "",
    "management_host_namespace": ""
  }
}
EOF
if "$topolab" --file "$legacy_topology" --working-dir "$working" up >"$test_dir/legacy-up.out" 2>&1; then
	echo "up silently migrated a legacy manifest" >&2
	exit 1
fi
grep -q 'legacy root datapath.*run topolab down' "$test_dir/legacy-up.out"
ip link show dev "$legacy_device" >/dev/null
"$topolab" --file "$legacy_topology" --working-dir "$working" down
if ip link show dev "$legacy_device" >/dev/null 2>&1; then
	echo "legacy owned interface remains after down" >&2
	exit 1
fi

snapshot_root > "$test_dir/root-after.json"
cmp "$test_dir/root-before.json" "$test_dir/root-after.json"
iptables -w -C OUTPUT -m comment --comment topolab-integration-sentinel -j "$sentinel_chain"
nft list table inet "$sentinel_table" | grep -q topolab-integration-sentinel

[ "$(sysctl -n net.ipv4.ip_forward)" = "$original_forward" ]
if ip -j -d link show | grep -q 'topolab:'; then
    echo "owned interfaces remain after down" >&2
    exit 1
fi

remove_sentinels
trap - EXIT HUP INT TERM
passed=true
rm -rf -- "$test_dir"
echo "TopoLab root network integration passed"
