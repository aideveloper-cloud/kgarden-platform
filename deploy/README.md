# K Garden on ServerKG

Production stack for the company Mac server **ServerKG** (Apple silicon, Colima, `server@100.102.142.13`
over Tailscale). One compose project, `kgarden`, next to the other KGF apps in `~/server/apps/kgarden`.

| Service | What | Memory cap |
|---|---|---|
| `db` | Postgres 17: the ERPNext site and `kgarden_mes` (MES + bridges) | 1 GB |
| `redis-cache`, `redis-queue` | ERPNext cache and job queue | 256 MB each |
| `frontend` → `backend` | ERPNext (nginx → gunicorn), `127.0.0.1:${ERP_PORT}` | 128 MB / 1.5 GB |
| `websocket`, `queue-short`, `queue-long`, `scheduler` | ERPNext realtime, background jobs, scheduler | ~2.2 GB |
| `mes`, `floor` | MES API and the floor tablet UI, `127.0.0.1:${MES_PORT}` | 256 MB / 64 MB |
| `cerp-bridge` | ConstructionERP → ERPNext; reads `kgf_construction` in `kgf-postgres` (network `kgf`) | 128 MB |
| `wms-bridge` | WMS → ERPNext (profile `wms`, `WMS_ENABLED=1`): needs the WMS MySQL on Tailscale | 128 MB |

Nothing listens beyond 127.0.0.1 (8010/8011 are for checks on the server). The public names come
from K Garden's own Cloudflare Tunnel `kgarden`, run by the `tunnel` service once `CF_TUNNEL_TOKEN`
is in `.env`. Its public hostnames, set in the dashboard: `erp.kgarden.co.th → http://frontend:8080`,
`mes.kgarden.co.th → http://floor:8080`. (`construction.kgarden.co.th` goes through `iso-tunnel`,
which is configured by a file, so dashboard hostnames do not apply to it.)

## Images

Built on the build Mac (arm64, same as the server) from **committed** code only:

```bash
deploy/build-erpnext.sh     # frappe + erpnext + kgarden at their HEAD commits -> kgarden/erpnext:<date>-<shas>, :latest
docker build --platform linux/arm64 -f deploy/platform/Dockerfile --target services -t kgarden/platform:latest .
docker build --platform linux/arm64 -f deploy/platform/Dockerfile --target floor -t kgarden/floor:latest .
deploy/kg.sh ship server@100.102.142.13     # docker save | ssh docker load, plus compose files
```

The image label `org.kgarden.commits` records which commits are inside (frappe and erpnext are on
`develop`: only ship commits that passed the e2e suite).

## First install (on ServerKG)

```bash
cd ~/server/apps/kgarden
cp .env.example .env && chmod 600 .env     # fill the secrets: openssl rand -hex 24; MES_ORG_ID: uuidgen
./kg.sh new-site          # Postgres, ERPNext site with erpnext + kgarden
./kg.sh setup-company     # setup wizard: COMPANY_NAME / COMPANY_ABBR / CHART_OF_ACCOUNTS (confirm with accounting)
./kg.sh provision         # service accounts, API keys, OAuth addresses -> .env; apps-credentials.env for CERP/WMS
./kg.sh up
```

ConstructionERP needs a read-only role for the bridge in `kgf-postgres` (`cerp_bridge`, `CERP_DB_URL`),
and its own `.env` gets the CERP values from `apps-credentials.env` (then delete that file).

## Updates

```bash
# build machine
deploy/build-erpnext.sh && deploy/kg.sh ship server@100.102.142.13
# server
./kg.sh backup && ./kg.sh migrate     # ERPNext changed
./kg.sh up                            # platform changed (migrations run first, in platform-migrate)
```

## Backups

`./kg.sh backup` writes the ERPNext backup (database + files) and a `kgarden_mes` dump to
`~/server/backups/kgarden`, keeping 14 days. Hook it into ServerKG's nightly `backup.sh`; the
server's off-box copy (`offbox.sh`) is not active yet, so nothing leaves the machine until it is.

## Rehearsal on the build Mac

The same compose file runs locally in Colima with another env file:

```bash
KG_ENV=/path/to/rehearsal.env deploy/kg.sh new-site   # etc.
```

Never point the e2e scripts at production: they create real documents.

## Rehearsal result (2026-11-23, this Mac, Colima arm64)

First install from an empty database (`new-site` → `setup-company` → `provision` → `up`), then an
update with `migrate`. With the seed data loaded into the rehearsal only, the e2e suites passed:
access 34, MES sign-in 15, production 15, CERP bridge 12, CERP AP 16, CERP retention 23. The WMS
suites wait for the droplet link. The whole stack used about 880 MB of memory.

## Known risks

- Frappe prints "PostgreSQL support is ... experimental" on `new-site`. We run Postgres and the
  suites pass, but upstream fixes land for MariaDB first.
- frappe and erpnext are on `develop`; ship only commits that passed the suites.
- The ERPNext backup includes `site_config` with the encryption key: keep backups as private as the server.
- Off-box copies are not set up on ServerKG yet.
