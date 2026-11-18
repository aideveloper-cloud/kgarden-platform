# K Garden Platform

Services that extend ERPNext for K Garden. ERPNext (with the `kgarden` Frappe app) stays the system of
record for items, BOMs, Work Orders, stock and accounts; these services own only the data they add and
talk to ERPNext through its REST API.

```
services/mes/         Production shop-floor service (Go): Work Order sync, start/stop, output, outbox to ERPNext
services/wms-bridge/  Copies the WMS (production-pos) stock ledger into ERPNext as Stock Entries (Go)
services/cerp-bridge/ Books ConstructionERP purchase orders in ERPNext, company KGF Construction (Go)
internal/             Shared Go packages: ERPNext REST client, PostgreSQL store and migrations
apps/floor/     Shop-floor tablet screen (SvelteKit, Bun, Tailwind) on top of the MES API
scripts/        End-to-end tests against a running ERPNext + services
```

## Production (MES) slice

1. ERPNext Work Orders are mirrored into `mes.mirror_work_order` (on start, every 60 s, or `POST /sync/work-orders`).
2. The floor starts/stops a job and reports output with an `Idempotency-Key` header; a retried request is a replay.
3. Good quantity goes into `mes.outbox_event` in the same transaction.
4. The worker builds a Manufacture Stock Entry with ERPNext's own `make_stock_entry` (BOM backflush),
   tags it with `kg_idempotency_key` (unique in ERPNext) and submits it. A retry finds the existing entry.
5. Materials ERPNext consumed are copied to `mes.material_consumption`.

Scrap is recorded in `mes.scrap_log` only; posting it to ERPNext is not done yet.

### API (`MES_HTTP_ADDR`, default 127.0.0.1:8100)

| Method | Path | |
|---|---|---|
| GET | `/work-orders` | open Work Orders with produced and pending qty |
| GET | `/work-orders/{name}` | detail, outputs, operation logs |
| POST | `/work-orders/{name}/start` | `{"operator": "..."}` |
| POST | `/work-orders/{name}/stop` | `{"operator": "..."}` |
| POST | `/work-orders/{name}/output` | `{"good_qty": 4, "scrap_qty": 0, "scrap_reason": "", "operator": "..."}` + `Idempotency-Key` header |
| POST | `/sync/work-orders` | pull Work Orders from ERPNext now |
| GET | `/outbox?status=pending\|sent\|dead` | outbox state, errors |

## App access, managed in ERPNext (`kgarden` app, `kgarden.access`)

People and their access to every K Garden app are managed in one place: the User form in ERPNext
(Roles & Permissions tab, "K Garden: สิทธิ์เข้าใช้แอป"). Decided 2026-11-18.

- **KG App** lists the apps (ERPNEXT desk, WMS, CERP, MES_FLOOR) with their roles and scope
  (WMS: Warehouse, CERP: Project). Each app has an access role `KG App: <code>`.
- A user's table **แอปที่เข้าใช้ได้** grants app + role (+ scope). Not in the table = cannot use the app.
  **ผู้ดูแลระบบสูงสุด** (`KG Super Admin`) opens every app as admin. Only a super admin may change
  these fields; everything is logged in the user's activity.
- Saving the user syncs Frappe roles: the access role of each granted app, the ERPNext roles of the
  desk app's role (no desk app = Website User, no desk), KG Super Admin + System Manager for a super admin.
- **Sign-in (OpenID Connect):** each app has an OAuth Client in ERPNext allowing only its access role
  and KG Super Admin, so ERPNext itself refuses to sign anyone else in (a Thai "ไม่มีสิทธิ์เข้าใช้" page).
  The userinfo (`frappe.integrations.oauth2.openid_profile`) adds `kg_app`, `kg_role`, `kg_scopes`,
  `kg_scope_doctype`, `kg_superadmin`, `kg_employee_code` for the app asking.
- **Losing access** (row removed, user disabled) revokes that app's OAuth tokens at once; apps poll
  `kgarden.access.api.access_changes` to end their own sessions.
- **Shared floor tablets:** employee code + PIN (`set_pin` by a super admin, stored as a hash in
  KG User PIN) checked by `kgarden.access.api.verify_pin` with the app's service account
  (role `KG Access Service`); rate-limited per employee code.
- Integration accounts (`integration@`, `cerp.integration@`) are not managed by this form.

```bash
ERPNEXT_ADMIN_PASSWORD=... scripts/e2e_access.sh     # 25 checks through the real sign-in flow (local site only)
ERPNEXT_ADMIN_PASSWORD=... scripts/e2e_mes_auth.sh   # 16 checks: MES PIN + ERPNext sign-in, revocation, roles
```

## WMS bridge (`services/wms-bridge`)

The WMS (production-pos, Laravel + MySQL) keeps the physical stock: units, spools, FIFO, PIN-locked
tablets. ERPNext is the core record of quantity and value. The bridge reads the WMS `stock_ledgers`
table with a **read-only** MySQL user and posts one submitted Stock Entry per ledger row, tagged
`kg_idempotency_key = wms-ledger-<id>`; a resend finds the existing entry instead of posting twice.
The WMS code is not changed.

### Production flow (decided 2026-11-18)

Raw material is issued to production **once**: the WMS moves it into WIP, the MES backflushes it.

```
WMS RM tablet "ตัด" / Spoolman filament use ──bridge──▶ Material Transfer  warehouse → WIP
MES output (tablet)                         ──MES────▶ Manufacture        WIP (BOM) → FG output warehouse
WMS FG tablet "รับเข้า"                      ──bridge──▶ Material Transfer  FG output warehouse → warehouse
```

The `kgarden` app enforces the middle step: every Work Order is forced to `skip_transfer = 1` and
backflushes all materials from the company's Default WIP Warehouse. An output reported before its
material reached WIP is not failed: the MES keeps retrying and the tablet shows "รอวัตถุดิบเข้า WIP";
the bridge does the same when the WMS receives finished goods before the MES reported them.

| WMS posting | ERPNext Stock Entry |
|---|---|
| RM leaving for production (`warehouse_floor_cut`, `spoolman_consumption_event`, CONSUMPTION) | Material Transfer warehouse → `WMS_BRIDGE_WIP_WAREHOUSE` |
| FG arriving from production (`warehouse_floor_receive` of an FG product) | Material Transfer `WMS_BRIDGE_FG_STAGING_WAREHOUSE` → warehouse |
| Other RECEIVE, COUNT/ADJUSTMENT (+) | Material Receipt (rate = WMS `buy_price`, zero allowed) |
| Other ISSUE (e.g. `supplier_return`, FG cut), COUNT/ADJUSTMENT (−) | Material Issue |
| TRANSFER_OUT / TRANSFER_IN | Material Transfer via the transit warehouse |

- Posting date/time is the WMS `posted_at` (WMS_TIMEZONE). ERPNext may queue a Repost Item
  Valuation for entries in the same second; its scheduler settles values (Bin) within the hour.
- WMS warehouses are matched by name (created under `WMS_BRIDGE_PARENT_WAREHOUSE` if missing); items by
  SKU = item code. Missing Items/UOMs/Warehouses are created only when `WMS_BRIDGE_CREATE_MASTERS=1`.
- The read cursor only passes rows older than 5 minutes, because MySQL auto-increment ids can become
  visible out of order; rows above it are re-read and deduplicated.
- Failures retry with backoff (2^n s, max 5 min) and go `dead` after 10 attempts; a missing-stock
  error (NegativeStockError) keeps retrying, since the movement that brings the stock may still come.

| Method | Path | |
|---|---|---|
| GET | `/summary` | counts by status, read cursor, warehouse mapping |
| GET | `/events?status=pending\|sent\|dead` | ledger events, errors, Stock Entry names |
| POST | `/events/{ledger_id}/retry` | put a dead event back after fixing the cause in ERPNext |

The ERPNext integration user needs Stock User + Stock Manager + Item Manager (for masters).

## ConstructionERP bridge (`services/cerp-bridge`)

Decided 2026-11-18: **ConstructionERP buys, ERPNext books**, and the construction business is its own
company in ERPNext (`KGF Construction`, abbr KGFC, set up by `kgarden.construction.setup.ensure`).
ConstructionERP keeps PR → PO → approval (Thai numbering, VAT, WHT, budget check); ERPNext owns
suppliers, projects, accounts, VAT and AP. The bridge reads ConstructionERP with a read-only role
(`cerp_bridge`) and uses its own ERPNext user `cerp.integration@example.com`.

| ConstructionERP | ERPNext |
|---|---|
| contact (vendor) | Supplier, matched by `kg_cerp_id`, else adopted by tax ID, else created |
| project | Project (`kg_cerp_id`) in KGF Construction |
| PO approved / issued / partially received / completed | one submitted Purchase Order (`kg_cerp_po_number`, `kg_idempotency_key`) |
| PO line `cost_category` | non-stock item CON-MATERIAL / LABOR / SUBCONTRACT / EQUIPMENT / OTHER; text, unit, qty, rate kept |
| `vat_mode` exclusive / inclusive / none | VAT row on `CERP_BRIDGE_VAT_ACCOUNT` (inclusive = included in rate) / none |
| WHT (type, rate, amount) | written in the PO terms; booked later on the purchase invoice / payment |

- Every PO version (content hash) gets its own key `cerp-po-<id>-<hash>`. An approved PO sent back
  to draft and edited, then approved again, cancels the old ERPNext PO and books a new one; draft or
  cancelled leaves no submitted ERPNext PO. Issuing or receiving does not create a new version.
- A PO is submitted only if ERPNext's grand total equals ConstructionERP's `amount_total`; otherwise it
  stays a draft with the difference in `last_error`.
- Polls `updated_at` every 5 s with a 5-minute look-back; failures back off and go `dead` after 10.
- API: `GET /purchase-orders?status=pending|synced|dead`, `POST /purchase-orders/{id}/retry`.

Receipts, invoices and payments (`cerp_bridge.doc_state`, posted after the POs they depend on):

| ConstructionERP | ERPNext |
|---|---|
| goods receipt `confirmed` | Purchase Receipt from the booked PO (mapper), accepted qty = received − rejected |
| payable invoice `approved`/`sent`/`paid` | Purchase Invoice: from the PO when linked (lines matched by text + rate), else on its own with VAT as an exact amount; submitted only if the total matches |
| payment on a payable invoice | Payment Entry: cash from the company cash account, Thai WHT as a deduction on `CERP_BRIDGE_WHT_ACCOUNT` (ภาษีหัก ณ ที่จ่ายค้างจ่าย), type / rate / certificate no. kept for ภ.ง.ด.3/53 |

- WHT is recorded on the payment in ConstructionERP (`wht_type`, `wht_rate`, `wht_amount`,
  `wht_certificate_no`, its "mark paid" dialog), decided 2026-11-18; cash + WHT settle the invoice.
- A cancelled invoice cancels its Purchase Invoice (ERPNext refuses while payments exist: that needs a person).
- Receipts and payments dated in the future wait until that day (ERPNext refuses future-dated documents).
- Not booked yet: invoices with retention, supplier refunds, credit notes.
- API: `GET /documents?kind=receipt|invoice|payment&status=...`, `POST /documents/{kind}/{id}/retry`.

### Signing in to the MES (`services/mes/auth.go`)

Who may use the floor tablet, and as what, is set in ERPNext (app MES_FLOOR: operator, supervisor;
super admins and Administrator get every app). Two ways in, both decided by ERPNext:

- **Employee code + PIN** on a shared tablet: `POST /auth/pin`, checked by
  `kgarden.access.api.verify_pin` with the MES service account (`MES_ACCESS_API_KEY/SECRET`, role
  KG Access Service only).
- **"เข้าสู่ระบบด้วย K Garden"**: OpenID Connect against ERPNext (`GET /auth/oidc/start`, ERPNext sign-in
  page, back to the tablet's `/auth/callback`, `POST /auth/oidc/callback`; the code is exchanged server
  side with `MES_OIDC_CLIENT_ID/SECRET`; the state is single use).

The MES then issues its own session (HttpOnly cookie, stored hashed in `mes.session`, `MES_SESSION_TTL`,
default 12h). Every API call except `/healthz` and `/auth/*` needs it; operators and supervisors work
the floor, only supervisors pull Work Orders and read the outbox. The operator of every action is the
signed-in person ("ชื่อ (รหัสพนักงาน)"), never a name sent in the request. Sessions end on logout, after
10 minutes without a touch on the tablet, or within `MES_ACCESS_POLL` (default 1m) when ERPNext reports
the person lost MES access or got another role.

### Tablet screen (`apps/floor`)

Operator signs in (employee code + PIN, or through ERPNext), picks an open Work Order, starts/stops the job and reports good and scrap
quantity on a large keypad (scrap needs a reason). Each report carries an `Idempotency-Key`; if the send
fails on the network or a 5xx, the values are locked and "ส่งซ้ำ" resends the same key, so a retry is never
counted twice. The screen refreshes every 3 s and shows whether each report has reached ERPNext.

```bash
cd apps/floor && bun install && bun run dev   # http://127.0.0.1:5180, /api proxied to MES_HTTP_ADDR
bun run build                                 # static SPA in apps/floor/build
```

## Local development

Needs the local bench (`erp.localhost`, PostgreSQL container on port 15432) and the `kgarden` app installed.
Copy `.env.example` to `.env` and fill it in (database roles `kgarden_mes_owner` / `kgarden_mes_app`, and the
ERPNext integration user's API key).

```bash
set -a; . ./.env; set +a
go build -o bin/ ./services/...
./bin/mes migrate && ./bin/wms-bridge migrate   # as kgarden_mes_owner
./bin/mes serve                                 # as kgarden_mes_app (row-level security applies)
./bin/wms-bridge serve
scripts/e2e_production.sh
WMS_APP_DIR=~/Desktop/production-pos/point-of-sales scripts/e2e_wms_bridge.sh   # local WMS copy only
WMS_APP_DIR=~/Desktop/production-pos/point-of-sales scripts/e2e_wip_flow.sh      # WMS → WIP → MES → WMS
./bin/cerp-bridge migrate && ./bin/cerp-bridge serve
CERP_URL=http://127.0.0.1:8200 scripts/e2e_cerp_bridge.sh                       # local ConstructionERP only
CERP_URL=http://127.0.0.1:8200 scripts/e2e_cerp_ap.sh                           # receipt → invoice → payment with WHT
scripts/reconcile.sh   # read-only: does ERPNext hold exactly what WMS, ConstructionERP and MES say happened?
```

Local WMS copy: MySQL container `kgarden-wms-mysql` on 127.0.0.1:13306 (the WMS `.env` points there),
read by the bridge as `kgarden_bridge` (SELECT on `stock_ledgers`, `products`, `warehouses` only).

Database rules: quantities are `numeric`, every table has `org_id` with row-level security
(`app.org_id` is set per transaction), the app role cannot delete, and `mes.audit_log` is append-only.
