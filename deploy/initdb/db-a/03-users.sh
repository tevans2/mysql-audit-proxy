#!/bin/bash
# Per-user accounts for db-a (ADR 0004). Runs after the schema files, once, on a
# fresh data directory.
#
# A shell script rather than SQL so the passwords come from the environment
# (deploy/.env, via docker-compose.yml) instead of being literals in a committed
# file. The entrypoint runs this against its own temporary server, so the root
# password it already holds is all the access needed.
#
#   alice            SELECT on sales.customers, and the routing marker. Nothing else.
#   proxy_bootstrap  GRANT USAGE only: it can log in and run SELECT 1, and every
#                    table gives 1142. It is the account the proxy opens an
#                    upstream connection with before COM_CHANGE_USER, so leaking
#                    it must grant nothing.
set -eu

for v in ALICE_PASSWORD BOOTSTRAP_PASSWORD; do
  value="${!v:-}"
  if [ -z "$value" ]; then
    echo "03-users.sh: $v is not set — see deploy/.env.example" >&2
    exit 1
  fi
  # Interpolated into a SQL string below, so the two characters that would end it.
  case "$value" in
    *\'* | *\\*) echo "03-users.sh: $v must not contain ' or \\" >&2; exit 1 ;;
  esac
done

MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot <<SQL
CREATE USER 'alice'@'%' IDENTIFIED WITH caching_sha2_password BY '${ALICE_PASSWORD}';
GRANT SELECT ON sales.customers TO 'alice'@'%';
GRANT SELECT ON auditproxy_test.upstream_marker TO 'alice'@'%';

CREATE USER 'proxy_bootstrap'@'%' IDENTIFIED WITH caching_sha2_password BY '${BOOTSTRAP_PASSWORD}';
GRANT USAGE ON *.* TO 'proxy_bootstrap'@'%';
SQL
