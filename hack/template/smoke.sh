#!/bin/sh
set -eu

[ "$#" -eq 1 ] || { echo "usage: $0 IMAGE.qcow2" >&2; exit 2; }
image=$1
case $image in /*) ;; *) image=$(pwd)/$image ;; esac
[ -f "$image" ] || { echo "image not found: $image" >&2; exit 2; }

ovmf_code=/usr/share/OVMF/OVMF_CODE_4M.fd
ovmf_vars=/usr/share/OVMF/OVMF_VARS_4M.fd
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/topolab-template-smoke.XXXXXX")
qemu_pid=
cleanup() {
    if [ -n "$qemu_pid" ] && kill -0 "$qemu_pid" 2>/dev/null; then
        kill -TERM "$qemu_pid" 2>/dev/null || true
        wait "$qemu_pid" 2>/dev/null || true
    fi
    rm -rf -- "$test_dir"
}
trap cleanup EXIT HUP INT TERM

ssh-keygen -q -t ed25519 -N '' -f "$test_dir/id"
public_key=$(sed -n '1p' "$test_dir/id.pub")
cat > "$test_dir/meta-data" <<'EOF'
instance-id: topolab-template-build-smoke
local-hostname: topolab-template-smoke
EOF
cat > "$test_dir/user-data" <<EOF
#cloud-config
ssh_deletekeys: false
users:
  - name: torob
    lock_passwd: true
    shell: /bin/bash
    groups: [sudo]
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - "$public_key"
EOF
cat > "$test_dir/network-config" <<'EOF'
version: 2
ethernets:
  smoke0:
    match:
      macaddress: "02:00:00:00:00:01"
    set-name: smoke0
    dhcp4: true
EOF
xorriso -as mkisofs -quiet -output "$test_dir/seed.iso" -volid cidata \
    -joliet -rock -graft-points \
    "meta-data=$test_dir/meta-data" "user-data=$test_dir/user-data" \
    "network-config=$test_dir/network-config"
xorriso -osirrox on -indev "$test_dir/seed.iso" \
    -extract /network-config "$test_dir/network-config.from-iso"
cmp "$test_dir/network-config" "$test_dir/network-config.from-iso"

qemu-img create -q -f qcow2 -F qcow2 -b "$image" "$test_dir/root.qcow2"
cp "$ovmf_vars" "$test_dir/OVMF_VARS.fd"
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')

qemu-system-x86_64 \
    -name guest=topolab:template:smoke \
    -enable-kvm -machine q35,accel=kvm -cpu host \
    -smp cpus=2,sockets=1,cores=2,threads=1 -m 2048 \
    -display none -monitor none -serial "file:$test_dir/serial.log" \
    -drive "if=pflash,format=raw,readonly=on,file=$ovmf_code" \
    -drive "if=pflash,format=raw,file=$test_dir/OVMF_VARS.fd" \
    -object iothread,id=io-root \
    -blockdev "driver=file,node-name=file-root,filename=$test_dir/root.qcow2" \
    -blockdev driver=qcow2,node-name=blk-root,file=file-root \
    -device virtio-scsi-pci,id=scsi-root,iothread=io-root \
    -device scsi-hd,drive=blk-root,bus=scsi-root.0,serial=topolab-smoke-root \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:$port-:22" \
    -device virtio-net-pci,netdev=net0,mac=02:00:00:00:00:01 \
    -drive "if=none,format=raw,readonly=on,id=seed,file=$test_dir/seed.iso" \
    -device ide-cd,drive=seed,id=seed-cd &
qemu_pid=$!

deadline=$(( $(date +%s) + 300 ))
until ssh -p "$port" -i "$test_dir/id" -o IdentitiesOnly=yes \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=3 torob@127.0.0.1 true >/dev/null 2>&1; do
    kill -0 "$qemu_pid" 2>/dev/null || { tail -200 "$test_dir/serial.log" >&2; exit 1; }
    [ "$(date +%s)" -lt "$deadline" ] || { tail -200 "$test_dir/serial.log" >&2; exit 1; }
    sleep 2
done

deadline=$(( $(date +%s) + 300 ))
while :; do
    cloud_status=$(timeout 10s ssh -p "$port" -i "$test_dir/id" -o IdentitiesOnly=yes \
        -o BatchMode=yes -o LogLevel=ERROR \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=3 torob@127.0.0.1 'cloud-init status' 2>/dev/null || true)
    printf '%s\n' "$cloud_status" | grep -q '^status: done' && break
	kill -0 "$qemu_pid" 2>/dev/null || { tail -200 "$test_dir/serial.log" >&2; exit 1; }
	if printf '%s\n' "$cloud_status" | grep -q '^status: error' || [ "$(date +%s)" -ge "$deadline" ]; then
		ssh -p "$port" -i "$test_dir/id" -o IdentitiesOnly=yes \
			-o BatchMode=yes -o LogLevel=ERROR \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
			torob@127.0.0.1 'cloud-init status --long; cloud-init query ds.network_config || true; sudo cat /var/lib/cloud/instance/network-config.json || true; sudo grep -R -n -E "config: enabled|config: disabled" /etc/cloud || true; sudo find /etc/netplan -maxdepth 1 -type f -print -exec cat {} \;; sudo grep -i -E "network.config|network-config|seed" /var/log/cloud-init.log | tail -100; sudo journalctl --no-pager -u cloud-final.service -n 100' >&2 || true
        exit 1
	fi
	sleep 2
done

timeout 60s ssh -p "$port" -i "$test_dir/id" -o IdentitiesOnly=yes \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null torob@127.0.0.1 \
    'set -eu; actual=; for path in /sys/class/net/*; do [ "$(cat "$path/address")" = 02:00:00:00:00:01 ] && actual=${path##*/}; done; test -n "$actual"; test "$(cat "/sys/class/net/$actual/mtu")" = 1500; ip -4 address show dev "$actual" | grep -q "inet "; sudo grep -q "set-name:.*smoke0" /etc/netplan/50-cloud-init.yaml; sudo grep -qi "02:00:00:00:00:01" /etc/netplan/50-cloud-init.yaml; test -s /etc/machine-id; ls /etc/ssh/ssh_host_*_key >/dev/null; for package in cloud-init openssh-client openssh-server curl ca-certificates iptables nftables iproute2 iputils-ping traceroute qemu-guest-agent dnsmasq; do status=$(dpkg-query -W -f=\${Status} "$package"); test "$status" = "install ok installed" || { echo "$package: $status" >&2; exit 1; }; done'

# A successful guest shutdown commonly closes SSH before it can deliver the
# remote exit status, so 255 is acceptable only for this separate poweroff.
shutdown_status=0
ssh -p "$port" -i "$test_dir/id" -o IdentitiesOnly=yes \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null torob@127.0.0.1 \
    'sudo systemctl poweroff' || shutdown_status=$?
[ "$shutdown_status" -eq 0 ] || [ "$shutdown_status" -eq 255 ] || exit "$shutdown_status"

deadline=$(( $(date +%s) + 60 ))
while kill -0 "$qemu_pid" 2>/dev/null; do
    [ "$(date +%s)" -lt "$deadline" ] || { echo "smoke VM did not power off" >&2; exit 1; }
    sleep 1
done
wait "$qemu_pid"
qemu_pid=
echo "TopoLab UEFI/VirtIO-SCSI/NoCloud template smoke passed"
