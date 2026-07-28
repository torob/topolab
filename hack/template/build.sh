#!/bin/sh
set -eu

BUILDER_VERSION=1.3.0
BUILDER_SHA256=22b2e6c33cd9522c1f7d31ac1a0e95a77d5dfdb56993e78f200dfa8d0b6a50fc
BUILDER_URL=https://github.com/torob/ubuntu-vm-template-builder/releases/download/v1.3.0/ubuntu-vm-template-builder-linux-amd64
ISO_URL=https://releases.ubuntu.com/26.04/ubuntu-26.04-live-server-amd64.iso
ISO_SHA256=dec49008a71f6098d0bcfc822021f4d042d5f2db279e4d75bdd981304f1ca5d9

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
output=${1:-$script_dir/ubuntu-26.04-topolab.qcow2}
case $output in
    /*) ;;
    *) output=$(pwd)/$output ;;
esac
[ ! -e "$output" ] || { echo "refusing to overwrite $output" >&2; exit 1; }

tools_root=${CODEX_TOOLS:-$HOME/.tools}
builder_dir=$tools_root/ubuntu-vm-template-builder/$BUILDER_VERSION
builder=$builder_dir/ubuntu-vm-template-builder
cache_dir=${XDG_CACHE_HOME:-$HOME/.cache}/topolab-template
iso=$cache_dir/ubuntu-26.04-live-server-amd64.iso
mkdir -p "$builder_dir" "$cache_dir" "$(dirname -- "$output")"

if [ ! -x "$builder" ]; then
    partial=$builder.partial.$$
    trap 'rm -f -- "$partial"' EXIT HUP INT TERM
    curl --fail --location --retry 3 --output "$partial" "$BUILDER_URL"
    printf '%s  %s\n' "$BUILDER_SHA256" "$partial" | sha256sum --check --status
    chmod 0755 "$partial"
    mv "$partial" "$builder"
    trap - EXIT HUP INT TERM
fi
printf '%s  %s\n' "$BUILDER_SHA256" "$builder" | sha256sum --check --status || {
    echo "builder checksum mismatch: $builder" >&2
    exit 1
}

if [ ! -f "$iso" ] || ! printf '%s  %s\n' "$ISO_SHA256" "$iso" | sha256sum --check --status; then
    partial=$iso.partial.$$
    trap 'rm -f -- "$partial"' EXIT HUP INT TERM
    curl --fail --location --retry 3 --output "$partial" "$ISO_URL"
    printf '%s  %s\n' "$ISO_SHA256" "$partial" | sha256sum --check --status
    mv "$partial" "$iso"
    trap - EXIT HUP INT TERM
fi

temporary_dir=$(mktemp -d "$(dirname -- "$output")/.topolab-template.XXXXXX")
temporary=$temporary_dir/image.qcow2
cleanup() { rm -f -- "$temporary"; rmdir -- "$temporary_dir" 2>/dev/null || true; }
trap cleanup EXIT HUP INT TERM

"$builder" qemu build \
    --iso "$iso" \
    --image "$temporary" \
    --disk-format qcow2 \
    --user-data "$script_dir/autoinstall.yaml" \
    --hardware-config "$script_dir/hardware.qemu.yaml" \
    --install-extra-packages "$script_dir/extra-packages.yaml"

qemu-img check "$temporary"
qemu-img info --output=json "$temporary" | python3 -c '
import json, sys
info = json.load(sys.stdin)
if info.get("format") != "qcow2": raise SystemExit("output is not qcow2")
if info.get("backing-filename"): raise SystemExit("output unexpectedly has a backing file")
if info.get("virtual-size") != 20 * 2**30: raise SystemExit("output capacity is not 20 GiB")
'

"$script_dir/smoke.sh" "$temporary"

# A same-filesystem hard link publishes without replacing a concurrently created file.
ln "$temporary" "$output" || { echo "output appeared during build; not overwriting $output" >&2; exit 1; }
rm -f -- "$temporary"
rmdir -- "$temporary_dir"
trap - EXIT HUP INT TERM
echo "$output"
