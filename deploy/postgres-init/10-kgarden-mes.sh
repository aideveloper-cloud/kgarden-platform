#!/bin/bash
# First start of the K Garden Postgres: roles and database for the MES and the bridges.
# kgarden_mes_owner runs migrations; kgarden_mes_app is what the services connect as (row-level
# security applies to it). ERPNext's own database and user are made by `bench new-site`.
set -euo pipefail
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" \
	-v owner_pw="$KG_MES_OWNER_PASSWORD" -v app_pw="$KG_MES_APP_PASSWORD" <<'SQL'
CREATE ROLE kgarden_mes_owner LOGIN PASSWORD :'owner_pw';
CREATE ROLE kgarden_mes_app LOGIN PASSWORD :'app_pw';
CREATE DATABASE kgarden_mes OWNER kgarden_mes_owner;
\c kgarden_mes
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT CONNECT ON DATABASE kgarden_mes TO kgarden_mes_app;
SQL
