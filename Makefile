# Lustre — common tasks. `make help` lists them.

COMPOSE := docker compose -f deploy/docker-compose.yml
# Compose reads the stack's secrets from here (gitignored). Every target that
# runs $(COMPOSE) depends on it, since Compose refuses to parse the file at
# all without it — even for `down`.
ENV     := deploy/.env
BIN     := bin
# mysql as root inside a db container, using that container's own
# MYSQL_ROOT_PASSWORD so the password isn't repeated here. MYSQL_PWD keeps it
# off the command line; "$$@" passes the caller's arguments through unquoted.
MYSQL_AS_ROOT := sh -c 'MYSQL_PWD="$$MYSQL_ROOT_PASSWORD" exec mysql -uroot "$$@"' mysql

# The e2e account passwords, read from deploy/.env so they are never literals
# here. ($$ keeps the shell, not make, expanding them.) .env is plain
# KEY='value' lines, so sourcing it is safe.
E2E_ACCOUNTS := . $(ENV) && E2E_DB_A_ALICE_PASS="$$DB_A_ALICE_PASSWORD" E2E_DB_A_BOOTSTRAP_PASS="$$DB_A_BOOTSTRAP_PASSWORD" E2E_DB_B_BOB_PASS="$$DB_B_BOB_PASSWORD" E2E_DB_B_BOOTSTRAP_PASS="$$DB_B_BOOTSTRAP_PASSWORD"

.PHONY: help build test lint fmt run-proxy run-api up ui down logs clean web-test test-env test-env-check edge-check csp-check routes test-e2e perf mysql-a mysql-b

help: ## List available targets
	@grep -hE '^[a-z0-9-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk -F':.*?## ' '{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build both binaries into bin/
	go build -o $(BIN)/proxy ./cmd/proxy
	go build -o $(BIN)/api ./cmd/api

test: web-test ## Run all tests; fails if the proxy's upstream is down (run test-env first)
	PROXY_TEST_REQUIRE_UPSTREAM=1 go test -count=1 -shuffle=on ./...

test-e2e: ## End-to-end tests against both upstreams (starts them if needed)
	@$(MAKE) --no-print-directory test-env
	$(E2E_ACCOUNTS) E2E_REQUIRE=1 go test -count=1 -v ./e2e/

perf: ## Performance baseline (measurements, not pass/fail — docs/perf/)
	@$(MAKE) --no-print-directory test-env
	@echo "Close what you can first: these numbers are only as quiet as the machine."
	PERF=1 E2E_REQUIRE=1 go test -count=1 -v -timeout 30m -run TestPerf ./e2e/

web-test: ## Unit tests for the UI's pure helpers (needs node 22+)
	node --test 'web/**/*.test.mjs'

lint: ## gofmt + go vet
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then echo "not gofmt'd:"; echo "$$files"; exit 1; fi
	go vet ./...

fmt: ## Rewrite files with gofmt
	gofmt -w .

run-proxy: ## Run the proxy against a local upstream
	go run ./cmd/proxy

run-api: ## Run the API against ./lustre.db
	go run ./cmd/api

# No prerequisite on .env.example on purpose: a newer template must never
# overwrite values someone has already put in their own .env.
$(ENV):
	cp deploy/.env.example $@
	@echo "created $@ from deploy/.env.example (development values; see deploy/README.md)"

test-env: $(ENV) ## Start just the two upstream databases (db-a, db-b)
	$(COMPOSE) up -d --remove-orphans db-a db-b
	@echo "waiting for both upstreams to report healthy..."
	@for i in $$(seq 1 60); do \
		a=$$($(COMPOSE) ps --format '{{.Health}}' db-a 2>/dev/null); \
		b=$$($(COMPOSE) ps --format '{{.Health}}' db-b 2>/dev/null); \
		if [ "$$a" = healthy ] && [ "$$b" = healthy ]; then break; fi; \
		sleep 2; \
	done
	@$(MAKE) --no-print-directory test-env-check

test-env-check: $(ENV) ## Verify each upstream answers as itself with its own schema
	@echo "db-a  (127.0.0.1:3306)"
	@$(COMPOSE) exec -T db-a $(MYSQL_AS_ROOT) -N -B \
		-e "SELECT CONCAT('  marker: ', name) FROM lustre_test.upstream_marker; \
		    SELECT CONCAT('  sales.customers rows: ', COUNT(*)) FROM sales.customers;" 2>/dev/null
	@echo "db-b  (127.0.0.1:3316)"
	@$(COMPOSE) exec -T db-b $(MYSQL_AS_ROOT) -N -B \
		-e "SELECT CONCAT('  marker: ', name) FROM lustre_test.upstream_marker; \
		    SELECT CONCAT('  hr.employees rows: ', COUNT(*)) FROM hr.employees;" 2>/dev/null
	@echo "both upstreams up and distinguishable"

up: $(ENV) ## Build and start the full stack
	# --remove-orphans: this stack's services have been renamed more than
	# once (proxy -> proxy-a/proxy-b -> proxy again, once it grew a listener
	# per upstream; db -> db-a). Without this, old containers from a previous
	# name survive and hold onto ports the new ones need, so they fail to bind.
	$(COMPOSE) up -d --build --remove-orphans

ui: $(ENV) ## Open the back-office UI (served by the edge; needs the stack up)
	@if ! curl -skf https://localhost/healthz >/dev/null 2>&1; then \
		echo "warning: nothing answering on https://localhost — run 'make up' first, or the page won't load"; \
	fi
	@echo "UI:  https://localhost   (a self-signed certificate warning is expected, once)"
	@if command -v open >/dev/null; then open https://localhost; \
	elif command -v xdg-open >/dev/null; then xdg-open https://localhost; fi

# csp-check first: it needs no stack and answers in milliseconds, so a broken
# page fails before we go asking a running edge about its headers.
edge-check: csp-check ## Assert the edge's security headers and cache policy (needs the stack up)
	@deploy/check-edge.sh

# The edge serves web/ under a strict CSP, so inline styles and scripts would
# be blocked at runtime. Fail here rather than in the browser.
csp-check: ## Fail if web/ has inline styles or scripts the CSP would block
	@fail=0; \
	if grep -rn 'style="' web/ --include='*.html' --include='*.js'; then \
	  echo "csp-check: inline style attribute in web/"; fail=1; fi; \
	if grep -rn '<style' web/ --include='*.html'; then \
	  echo "csp-check: inline <style> block in web/"; fail=1; fi; \
	if grep -rn '<script' web/ --include='*.html' | grep -v 'src='; then \
	  echo "csp-check: inline <script> in web/"; fail=1; fi; \
	if [ $$fail -eq 0 ]; then echo "csp-check: web/ is CSP clean"; fi; \
	exit $$fail

routes: $(ENV) ## Verify each proxy port reaches its own upstream (needs the full stack)
	@set -a; . $(ENV); set +a; fail=0; \
	for tuple in "3307 db-a alice DB_A_ALICE_PASSWORD" "3308 db-b bob DB_B_BOB_PASSWORD"; do \
		set -- $$tuple; port=$$1; want=$$2; user=$$3; pw=$$(printenv $$4); \
		got=$$($(COMPOSE) exec -T -e MYSQL_PWD="$$pw" client mysql -h proxy-$${want#db-} -P $$port \
			-u $$user -N -B \
			-e "SELECT name FROM lustre_test.upstream_marker;" 2>/dev/null | tr -d '\r'); \
		if [ "$$got" = "$$want" ]; then \
			echo "  :$$port -> $$got"; \
		else \
			echo "  :$$port -> got '$$got', want '$$want'"; fail=1; \
		fi; \
	done; \
	if [ $$fail -ne 0 ]; then echo "routing FAILED"; exit 1; fi; \
	echo "each proxy port reaches its own upstream"

mysql-a: $(ENV) ## MySQL shell on db-a through the proxy (3307), as alice
	@set -a; . $(ENV); set +a; $(COMPOSE) exec -e MYSQL_PWD="$$DB_A_ALICE_PASSWORD" client mysql -h proxy-a -P 3307 -u alice

mysql-b: $(ENV) ## MySQL shell on db-b through the proxy (3308), as bob
	@set -a; . $(ENV); set +a; $(COMPOSE) exec -e MYSQL_PWD="$$DB_B_BOB_PASSWORD" client mysql -h proxy-b -P 3308 -u bob

down: $(ENV) ## Stop the stack, keeping the audit database
	$(COMPOSE) down

logs: $(ENV) ## Follow proxy and API logs
	$(COMPOSE) logs -f proxy api

clean: ## Remove built binaries and the local audit database
	rm -rf $(BIN) lustre.db lustre.db-shm lustre.db-wal

.PHONY: test-tls test-tls-down
test-tls: ## Mandatory TLS acceptance checks in an isolated MySQL stack (set TLS_TEST_PASSWORD)
	./deploy/tls-test/generate-certs.sh
	docker compose -f deploy/tls-test/compose.yml up -d --wait
	TLS_E2E_REQUIRE=1 TLS_E2E_CA_FILE=$(CURDIR)/deploy/tls-test/certs/ca.pem go test -count=1 -v ./e2e -run '^TestUpstreamTLS$$'

test-tls-down: ## Remove only the disposable TLS acceptance stack
	docker compose -f deploy/tls-test/compose.yml down -v
	