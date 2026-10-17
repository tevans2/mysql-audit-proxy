#!/bin/sh
# Disposable certificates for the isolated TLS acceptance environment only.
set -eu
cd "$(dirname "$0")"
mkdir -p certs
if [ -f certs/ca.pem ] && [ -f certs/db-a.pem ] && [ -f certs/db-b.pem ]; then
  exit 0
fi
umask 077
openssl req -x509 -newkey rsa:2048 -nodes -days 7 -subj /CN=Lustre-TLS-test-CA \
  -keyout certs/ca-key.pem -out certs/ca.pem 2>/dev/null
for name in db-a db-b; do
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$name" \
    -keyout "certs/$name-key.pem" -out "certs/$name.csr" 2>/dev/null
  cat > "certs/$name.ext" <<EXT
subjectAltName=DNS:$name,DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EXT
  openssl x509 -req -in "certs/$name.csr" -CA certs/ca.pem -CAkey certs/ca-key.pem \
    -CAcreateserial -days 7 -extfile "certs/$name.ext" -out "certs/$name.pem" 2>/dev/null
  # MySQL runs as a container UID; these disposable keys must be readable there.
  chmod 644 "certs/$name-key.pem" "certs/$name.pem"
done
chmod 644 certs/ca.pem
