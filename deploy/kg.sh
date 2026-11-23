#!/usr/bin/env bash
# Operate the K Garden stack (deploy/compose.yaml). Run on the machine that runs the stack
# (ServerKG, or this Mac for a local rehearsal); `ship` runs on the build machine.
#
#   kg.sh up                     start / update everything (bridges only with WMS_ENABLED / CERP_ENABLED=1)
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
profiles=()
[[ ${WMS_ENABLED:-0} == 1 ]] && profiles+=(--profile wms)
[[ ${CERP_ENABLED:-0} == 1 ]] && profiles+=(--profile cerp)
dc() { docker compose --env-file "$ENV_FILE" "${profiles[@]}" "$@"; }
site_bench() { dc exec -T backend bench --site "$ERP_SITE" "$@"; }

cmd=${1:-}; shift || true
case "$cmd" in
up)
	dc up -d --remove-orphans
	dc ps --format 'table {{.Service}}\t{{.Status}}'
	;;
new-site)
	dc up -d db redis-cache redis-queue configurator backend
	dc exec -T backend bench new-site "$ERP_SITE" \
		--db-type postgres --db-host db --db-root-username postgres --db-root-password "$PG_SUPERUSER_PASSWORD" \
		--admin-password "$ERP_ADMIN_PASSWORD" --install-app erpnext --install-app kgarden
	site_bench set-config host_name "$ERP_PUBLIC_URL"
	site_bench set-config -p serve_default_site 1
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
	dc exec -T db pg_dump -U postgres -Fc kgarden_mes > "$dir/kgarden_mes-$day.dump"
	find "$dir" -mindepth 1 -maxdepth 1 -mtime +"${RETAIN_DAYS:-14}" -exec rm -rf {} +
	echo "backup in $dir"
	;;
bench) site_bench "$@" ;;
status) dc ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}' ;;
logs) dc logs --tail 200 -f "$@" ;;
ship)
	target=${1:?ssh target, e.g. server@100.102.142.13}
	for img in kgarden/erpnext:latest kgarden/platform:latest kgarden/floor:latest; do
		echo "== $img"; docker save "$img" | gzip -1 | ssh "$target" 'export PATH=/opt/homebrew/bin:$PATH; gunzip | docker load'
	done
	ssh "$target" 'mkdir -p ~/server/apps/kgarden/postgres-init'
	scp -q compose.yaml kg.sh .env.example "$target:server/apps/kgarden/"
	scp -q postgres-init/* "$target:server/apps/kgarden/postgres-init/"
	echo "shipped; on the server: cd ~/server/apps/kgarden && ./kg.sh up   (or migrate after an ERPNext update)"
	;;
*) sed -n '2,15p' "$0"; exit 2 ;;
esac
