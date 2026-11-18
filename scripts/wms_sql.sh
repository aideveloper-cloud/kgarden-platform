#!/usr/bin/env bash
# Read-only query against the WMS database with the bridge's SELECT-only user (WMS_DB_DSN).
set -euo pipefail
dsn=${WMS_DB_DSN:?}
user=${dsn%%:*}; pw=${dsn#*:}; pw=${pw%%@tcp(*}
host=${dsn#*@tcp(}; host=${host%%:*}; port=${dsn#*@tcp(*:}; port=${port%%)*}
db=${dsn#*)/}; db=${db%%\?*}
MYSQL_PWD=$pw mysql --default-character-set=utf8mb4 -h "$host" -P "$port" -u "$user" "$db" -N -e "$1" 2> >(grep -v ssl-verify >&2)
