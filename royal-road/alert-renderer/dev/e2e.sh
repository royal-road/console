#!/bin/sh
# End-to-end test: the alert-renderer binary against fake vmalert, vmselect
# and Discord (dev/fake_backends.py), over HTTP, with the example payloads.
#
#   dev/e2e.sh path/to/alert-renderer [CAPTURE_DIR]
set -eu
bin=$1
here=$(cd "$(dirname "$0")" && pwd)
capture=${2:-$(mktemp -d)}
work=$(mktemp -d)
fake_port=18900
pids=""
cleanup() { for p in $pids; do kill "$p" 2>/dev/null || true; done; rm -rf "$work"; }
trap cleanup EXIT

python3 -I "$here/fake_backends.py" $fake_port "$capture" 2>"$work/fake.log" &
pids="$pids $!"

# One renderer per Discord behaviour: name port.
start() {
	printf 'http://127.0.0.1:%s/api/webhooks/1/%s\n' $fake_port "$1" >"$work/$1.url"
	DISCORD_WEBHOOK_URL_FILE="$work/$1.url" DISCORD_ALLOW_HTTP=true LISTEN_ADDR="127.0.0.1:$2" \
		VMALERT_URL="http://127.0.0.1:$fake_port/vmalert" \
		VMSELECT_URL="http://127.0.0.1:$fake_port/select/0/prometheus" \
		CONSOLE_URL=https://console.example.com "$bin" 2>"$work/$1.log" &
	pids="$pids $!"
}
start ok 18901
start reject-images 18902
start rate-limit 18903
start down 18904
start slow 18905
sleep 1

fails=0
expect() { # name want got
	if [ "$2" = "$3" ]; then echo "ok   $1 ($3)"; else echo "FAIL $1: want $2, got $3"; fails=$((fails + 1)); fi
}
post() { # port file
	curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' --data-binary "@$2" "http://127.0.0.1:$1/alertmanager"
}

expect healthz 200 "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18901/healthz)"
n=0
for p in "$here"/../testdata/payloads/*.json; do
	n=$((n + 1))
	expect "$(basename "$p")" 200 "$(post 18901 "$p")"
	expect "$(basename "$p") has a message" true "$([ -s "$capture/$n/payload_json.json" ] && echo true || echo false)"
done
expect "ingress chart attached" PNG "$(dd if="$capture/4/chart-0.png" bs=1 skip=1 count=3 2>/dev/null)"

payload="$here/../testdata/payloads/ingress-5xx-firing.json"
expect "Discord refuses images: text only" 200 "$(post 18902 "$payload")"
n=$((n + 1))
expect "  no image in the retry" false "$([ -e "$capture/$n/chart-0.png" ] && echo true || echo false)"
expect "  no image reference left" 0 "$(grep -c attachment:// "$capture/$n/payload_json.json" || true)"
expect "Discord rate limits: Alertmanager retries" 502 "$(post 18903 "$payload")"
expect "Discord down: Alertmanager retries" 502 "$(post 18904 "$payload")"
# A burst larger than the two rendering slots queues instead of failing.
burst=""
for i in 1 2 3 4 5 6; do
	post 18901 "$payload" >"$work/burst.$i" &
	burst="$burst $!"
done
for p in $burst; do wait "$p"; done
expect "burst of 6: all sent" 200200200200200200 "$(cat "$work"/burst.*)"
n=$((n + 6))
# Discord hangs: answered before Alertmanager's 30 s timeout.
t0=$(date +%s)
code=$(post 18905 "$payload")
expect "Discord hangs: Alertmanager retries" 502 "$code"
expect "  answered within 26 s" true "$([ $(($(date +%s) - t0)) -le 26 ] && echo true || echo false)"
expect "not a payload" 400 "$(curl -s -o /dev/null -w '%{http_code}' --data-binary 'nope' http://127.0.0.1:18901/alertmanager)"
head -c 1100000 /dev/zero | tr '\0' 'a' >"$work/big"
expect "oversized payload" 413 "$(curl -s -o /dev/null -w '%{http_code}' --data-binary "@$work/big" http://127.0.0.1:18901/alertmanager)"
expect "unknown path" 404 "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18901/)"
expect "webhook URL never logged" 0 "$(cat "$work"/ok.log "$work"/reject-images.log "$work"/rate-limit.log "$work"/down.log "$work"/slow.log | grep -c '/api/webhooks/' || true)"

echo "captured messages in $capture"
[ $fails -eq 0 ]
