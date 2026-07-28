#!/bin/sh
set -eu

[ "$#" -ge 1 ] && [ "$#" -le 2 ] || { echo "usage: $0 PRIVATE_KEY [all|routed|internet|service-internet]" >&2; exit 2; }
key=$1
mode=${2:-all}
case $mode in
    all|routed|internet|service-internet) ;;
    *) echo "invalid verification mode: $mode" >&2; exit 2 ;;
esac
client=203.0.113.10

ssh_client() {
    ssh -i "$key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
        -o BatchMode=yes -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 "torob@$client" "$@"
}

client_key=$(ssh_client 'umask 077; mktemp /tmp/topolab-routed-acceptance-key.XXXXXX')

cleanup() {
    ssh_client "rm -f -- '$client_key'" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

ssh_client "umask 077; dd of='$client_key' status=none" < "$key"

if [ "$mode" = all ] || [ "$mode" = routed ]; then
    address=192.0.2.10
    ssh_client "ping -c 2 -W 2 '$address'"
    connection=$(ssh_client \
        "ssh -i '$client_key' -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 'torob@$address' 'printf %s \"\$SSH_CONNECTION\"'")
    case $connection in
        203.0.113.10\ *) ;;
        *) echo "unexpected client source for $address: $connection" >&2; exit 1 ;;
    esac

    trace=$(ssh_client 'sudo traceroute -T -p 22 -n -m 5 192.0.2.10')
    printf '%s\n' "$trace"
    printf '%s\n' "$trace" | sed -n '2p' | grep -q '203\.0\.113\.1'
    printf '%s\n' "$trace" | sed -n '3p' | grep -q '198\.51\.100\.2'
fi

if [ "$mode" = all ] || [ "$mode" = internet ] || [ "$mode" = service-internet ]; then
    if [ "$mode" != service-internet ]; then
        ssh_client 'ip route get 1.1.1.1 >/dev/null && ping -c 2 -W 2 1.1.1.1 >/dev/null && getent ahostsv4 google.com >/dev/null && curl -fsSIL --connect-timeout 5 --max-time 15 https://www.google.com/ >/dev/null'
    fi
    for address in 198.51.100.2 192.0.2.10; do
        ssh_client \
            "ssh -i '$client_key' -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 'torob@$address' 'ip route get 1.1.1.1 >/dev/null && ping -c 2 -W 2 1.1.1.1 >/dev/null && getent ahostsv4 google.com >/dev/null && curl -fsSIL --connect-timeout 5 --max-time 15 https://www.google.com/ >/dev/null'"
    done
fi

cleanup
trap - EXIT HUP INT TERM

echo "synthetic routed $mode connectivity verified"
