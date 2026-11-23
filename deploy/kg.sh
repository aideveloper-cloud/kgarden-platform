#!/usr/bin/env bash
# Operate the K Garden stack (deploy/compose.yaml). Run on the machine that runs the stack
# (ServerKG, or this Mac for a local rehearsal); `ship` runs on the build machine.
#
#   kg.sh up                     start / update everything (bridges only with WMS_ENABLED / CERP_ENABLED=1)
#   kg.sh db-init                first install: database + roles for the MES/bridges in kgf-postgres
#   kg.sh new-site               first install: create the ERPNext site with erpnext + kgarden
#   kg.sh setup-company          first install: setup wizard (COMPANY_NAME, COMPANY_ABBR, ...)
#   kg.sh provision [--rotate]   service accounts + API keys + OAuth addresses -> written into the
#                                env file; keys for ConstructionERP / WMS go to apps-credentials.env
#   kg.sh migrate                after a new ERPNext image: bench migrate + restart
#   kg.sh backup [dir]           ERPNext backup (DB + files) and a dump of kgarden_mes
#   kg.sh bench <args...>        run bench for the site, e.g.  kg.sh bench console
#   kg.sh status | logs [svc]
#   kg.sh ship <ssh-target>      (build machine) send the images and deploy files to the server
#
# Env file: deploy/.env (KG_ENV=other-file to use another, e.g. a local rehearsal).
set -euo pipefail
cd "$(dirname "$0")"
ENV_FILE=${KG_ENV:-.env}
[[ $ENV_FILE == /* ]] || ENV_FILE=$PWD/$ENV_FILE
[[ -f $ENV_FILE ]] || { echo "missing $ENV_FILE (copy .env.example)"; exit 1; }
set -a; . "$ENV_FILE"; set +a
# ServerKG also has Docker Desktop: the KGF stack (kgf-postgres, network kgf) is in Colima (KGF deploy guide).
export DOCKER_CONTEXT=${KG_DOCKER_CONTEXT:-colima}
DB_HOST=${DB_HOST:-kgf-postgres}
profiles=()
[[ ${WMS_ENABLED:-0} == 1 ]] && profiles+=(--profile wms)
[[ ${CERP_ENABLED:-0} == 1 ]] && profiles+=(--profile cerp)
[[ -n ${CF_TUNNEL_TOKEN:-} ]] && profiles+=(--profile tunnel)
[[ $DB_HOST == db ]] && profiles+=(--profile localdb)
dc() { docker compose --env-file "$ENV_FILE" ${profiles[@]+"${profiles[@]}"} "$@"; }  # bash 3.2-safe
site_bench() { dc exec -T backend bench --site "$ERP_SITE" "$@"; }
# psql as the database's admin, inside its own container (the admin password never leaves it).
db_admin() {
	if [[ $DB_HOST == db ]]; then dc exec -T db psql -U postgres -v ON_ERROR_STOP=1 "$@"
	else docker exec -i "$DB_HOST" sh -c 'psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 "$@"' psql "$@"; fi
}
db_admin_cred() { # user and password for bench new-site
	if [[ $DB_HOST == db ]]; then printf '%s\n%s\n' postgres "$PG_SUPERUSER_PASSWORD"
	else docker exec "$DB_HOST" sh -c 'printf "%s\n%s\n" "$POSTGRES_USER" "$POSTGRES_PASSWORD"'; fi
}

cmd=${1:-}; shift || true
case "$cmd" in
up)
	dc up -d --remove-orphans
	dc ps --format 'table {{.Service}}\t{{.Status}}'
	;;
db-init)
	[[ $DB_HOST == db ]] && { dc up -d --wait db; }
	# Idempotent: roles get (re)set passwords from .env; the database is created once.
	db_admin -v owner_pw="$KG_MES_OWNER_PASSWORD" -v app_pw="$KG_MES_APP_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN', r) FROM unnest(ARRAY['kgarden_mes_owner','kgarden_mes_app']) r
	WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) \gexec
ALTER ROLE kgarden_mes_owner PASSWORD :'owner_pw';
ALTER ROLE kgarden_mes_app PASSWORD :'app_pw';
SELECT 'CREATE DATABASE kgarden_mes OWNER kgarden_mes_owner'
	WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'kgarden_mes') \gexec
\c kgarden_mes
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE CONNECT ON DATABASE kgarden_mes FROM PUBLIC;
GRANT CONNECT ON DATABASE kgarden_mes TO kgarden_mes_owner, kgarden_mes_app;
SQL
	echo "database kgarden_mes ready in $DB_HOST"
	;;
new-site)
	dc up -d redis-cache redis-queue configurator backend
	{ read -r root_user; read -r root_pw; } < <(db_admin_cred)
	dc exec -T backend bench new-site "$ERP_SITE" --db-name "${ERP_DB_NAME:-kgarden_erp}" \
		--db-type postgres --db-host "$DB_HOST" --db-root-username "$root_user" --db-root-password "$root_pw" \
		--admin-password "$ERP_ADMIN_PASSWORD" --install-app erpnext --install-app kgarden
	unset root_pw
	# Long-lived processes cache site_config for 60 s; one that cached it before the site's
	# encryption_key existed would generate a new key and break every stored secret. Start fresh.
	dc up -d --force-recreate backend
	site_bench set-config host_name "$ERP_PUBLIC_URL"
	site_bench set-config -p serve_default_site 1
	# kgarden's after_migrate (apps + OAuth clients, employee-code sign-in) does not run on install.
	site_bench execute kgarden.setup.after_migrate
	site_bench enable-scheduler
	dc exec -T backend bench use "$ERP_SITE"
	dc up -d
	echo "site $ERP_SITE ready: finish the setup wizard at $ERP_PUBLIC_URL (Administrator)"
	;;
setup-company)
	site_bench execute kgarden.deploy.setup_company --kwargs "$(python3 -c 'import json,os; print(json.dumps({
		"company": os.environ["COMPANY_NAME"], "abbr": os.environ["COMPANY_ABBR"],
		"admin_email": os.environ.get("ADMIN_EMAIL", "admin@example.com"),
		"language": os.environ.get("COMPANY_LANGUAGE", "th"),
		"chart_of_accounts": os.environ.get("CHART_OF_ACCOUNTS", "Standard")}))')"
	;;
provision)
	rotate=0; [[ ${1:-} == --rotate ]] && rotate=1
	urls=$(python3 -c 'import json,os; print(json.dumps({"MES_FLOOR": os.environ["MES_PUBLIC_URL"],
		"CERP": os.environ.get("CERP_PUBLIC_URL", ""), "WMS": os.environ.get("WMS_PUBLIC_URL", "")}))')
	out=$(site_bench execute kgarden.deploy.provision --kwargs "$(python3 -c 'import json,sys; print(json.dumps({"urls": sys.argv[1], "rotate": int(sys.argv[2])}))' "$urls" "$rotate")" | tail -1)
	# bench prints the returned JSON string; write env values in place, app credentials to a 600 file.
	python3 - "$ENV_FILE" "$out" <<'PY'
import json, os, re, sys
path, raw = sys.argv[1], sys.argv[2]
raw = raw.strip()
data = None
for cand in (raw, '"' + raw + '"'):  # bench prints the returned JSON string, quoted or not
	try:
		v = json.loads(cand)
		v = json.loads(v) if isinstance(v, str) else v
	except ValueError:
		continue
	if isinstance(v, dict):
		data = v
		break
if data is None:
	sys.exit("could not read the provision result")
text = open(path).read()
for k, v in data["env"].items():
	line = f"{k}='{v}'"
	text, n = re.subn(rf"^{k}=.*$", lambda m: line, text, flags=re.M)
	if not n:
		text += ("" if text.endswith("\n") else "\n") + line + "\n"
new = text
with open(path, "w") as f:
	f.write(new)
apps = os.path.join(os.path.dirname(os.path.abspath(path)), "apps-credentials.env")
fd = os.open(apps, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
	f.write("# Give these to ConstructionERP / WMS configuration; then delete this file.\n")
	f.writelines(f"{k}='{v}'\n" for k, v in data["apps"].items())
print("updated", path, "and wrote", apps)
PY
	# Every key must work now; a site whose encryption key changed underneath fails here, not later.
	set -a; . "$ENV_FILE"; set +a
	bad=0
	for pair in "MES_ERPNEXT_API_KEY:MES_ERPNEXT_API_SECRET" "MES_ACCESS_API_KEY:MES_ACCESS_API_SECRET" "CERP_ERPNEXT_API_KEY:CERP_ERPNEXT_API_SECRET"; do
		k=${pair%%:*}; v=${pair#*:}
		code=$(dc exec -T backend curl -s -o /dev/null -w '%{http_code}' -H "Host: $ERP_SITE" \
			-H "Authorization: token ${!k}:${!v}" http://frontend:8080/api/method/frappe.auth.get_logged_user)
		[[ $code == 200 ]] && echo "  key $k works" || { echo "  key $k refused ($code)"; bad=1; }
	done
	[[ $bad == 0 ]] || { echo "API keys do not work: run ./kg.sh provision --rotate, then ./kg.sh up"; exit 1; }
	;;
migrate)
	dc up -d configurator backend
	site_bench migrate
	dc up -d --force-recreate backend websocket frontend queue-short queue-long scheduler
	;;
backup)
	dir=${1:-$HOME/server/backups/kgarden}
	mkdir -p "$dir"; day=$(date +%F)
	site_bench backup --with-files
	cid=$(dc ps -q backend)
	docker cp "$cid:/home/frappe/frappe-bench/sites/$ERP_SITE/private/backups/." "$dir/erpnext-$day/"
	# (kgf-postgres is also in ServerKG's nightly pg_dumpall; this keeps a copy next to the files.)
	if [[ $DB_HOST == db ]]; then dc exec -T db pg_dump -U postgres -Fc kgarden_mes
	else docker exec "$DB_HOST" sh -c 'pg_dump -U "$POSTGRES_USER" -Fc kgarden_mes'; fi > "$dir/kgarden_mes-$day.dump"
	find "$dir" -mindepth 1 -maxdepth 1 -mtime +"${RETAIN_DAYS:-14}" -exec rm -rf {} +
	echo "backup in $dir"
	;;
bench) site_bench "$@" ;;
status) dc ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}' ;;
logs) dc logs --tail 200 -f "$@" ;;
ship)
	target=${1:?ssh target, e.g. server@100.102.142.13}
	for img in kgarden/erpnext:latest kgarden/platform:latest kgarden/floor:latest; do
		echo "== $img"; docker save "$img" | gzip -1 | ssh "$target" 'export PATH=/opt/homebrew/bin:$PATH DOCKER_CONTEXT=colima; gunzip | docker load'
	done
	ssh "$target" 'mkdir -p ~/server/apps/kgarden'
	scp -q compose.yaml kg.sh .env.example README.md "$target:server/apps/kgarden/"
	echo "shipped; on the server: cd ~/server/apps/kgarden && ./kg.sh up   (or migrate after an ERPNext update)"
	;;
*) sed -n '2,15p' "$0"; exit 2 ;;
esac
