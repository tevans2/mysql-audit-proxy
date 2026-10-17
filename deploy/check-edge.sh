#!/bin/sh
# Asserts the edge sends its security headers and cache policy. Needs the
# stack up (make up). Usage: deploy/check-edge.sh [base-url]
base="${1:-https://localhost}"
fail=0

# check PATH HEADER EXPECTED-SUBSTRING
check() {
	got=$(curl -skI "$base$1" | tr -d '\r' | grep -i "^$2:" | head -n1)
	case "$got" in
	*"$3"*) ;;
	*) echo "FAIL $1: want $2 containing '$3', got '${got:-<missing>}'"; fail=1 ;;
	esac
}

for p in / /styles.css /api/servers /healthz /api/nope; do
	check "$p" Strict-Transport-Security "max-age=31536000"
	check "$p" X-Content-Type-Options "nosniff"
	check "$p" Referrer-Policy "no-referrer"
	check "$p" Content-Security-Policy "default-src 'self'; frame-ancestors 'none'"
done
check / Cache-Control "no-store"
check /api/servers Cache-Control "no-store"
check /styles.css Cache-Control "no-cache"

[ "$fail" = 0 ] && echo "edge headers ok"
exit "$fail"
