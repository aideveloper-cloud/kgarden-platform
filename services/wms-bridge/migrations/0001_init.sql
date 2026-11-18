-- WMS -> ERPNext bridge. The WMS (production-pos, MySQL) keeps the physical stock ledger; every ledger
-- row is copied here and posted to ERPNext as one Stock Entry, tagged with kg_idempotency_key
-- 'wms-ledger-<id>' so a resend never posts twice.

CREATE SCHEMA wms_bridge;

-- Ledger rows read from the WMS. Rows are only inserted (ON CONFLICT DO NOTHING) and then move
-- pending -> sent | dead. The WMS values are kept as read, for audit.
CREATE TABLE wms_bridge.ledger_event (
	org_id          uuid NOT NULL,
	ledger_id       bigint NOT NULL,
	movement_type   text NOT NULL,
	sku             text NOT NULL,
	domain          text NOT NULL DEFAULT '',          -- WMS product domain: RM or FG
	product_title   text NOT NULL,
	wms_warehouse   text NOT NULL,          -- WMS warehouse code
	qty             numeric(21, 9) NOT NULL, -- signed: + in, - out (WMS base UOM)
	uom             text NOT NULL,
	unit_cost       numeric(21, 9) NOT NULL DEFAULT 0,
	posted_at       timestamptz NOT NULL,
	reference_type  text NOT NULL DEFAULT '',
	reference_id    bigint,
	operator        text NOT NULL DEFAULT '',
	notes           text NOT NULL DEFAULT '',
	status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'dead')),
	attempts        int NOT NULL DEFAULT 0,
	next_attempt_at timestamptz NOT NULL DEFAULT now(),
	last_error      text NOT NULL DEFAULT '',
	stock_entry     text,
	read_at         timestamptz NOT NULL DEFAULT now(),
	sent_at         timestamptz,
	PRIMARY KEY (org_id, ledger_id)
);
CREATE INDEX ledger_event_due ON wms_bridge.ledger_event (status, next_attempt_at, ledger_id);

-- WMS warehouse code -> ERPNext warehouse. Filled automatically the first time a code is seen.
CREATE TABLE wms_bridge.warehouse_map (
	org_id            uuid NOT NULL,
	wms_code          text NOT NULL,
	wms_name          text NOT NULL,
	erpnext_warehouse text NOT NULL,
	created_at        timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (org_id, wms_code)
);

-- How far the WMS ledger has been read for certain. Rows above it are re-read every poll, because
-- MySQL auto-increment ids can become visible out of order while transactions commit.
CREATE TABLE wms_bridge.read_cursor (
	org_id     uuid PRIMARY KEY,
	low_water  bigint NOT NULL DEFAULT 0,
	updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION wms_bridge.current_org() RETURNS uuid
LANGUAGE sql STABLE AS $$ SELECT nullif(current_setting('app.org_id', true), '')::uuid $$;

DO $$
DECLARE t text;
BEGIN
	FOREACH t IN ARRAY ARRAY['ledger_event', 'warehouse_map', 'read_cursor'] LOOP
		EXECUTE format('ALTER TABLE wms_bridge.%I ENABLE ROW LEVEL SECURITY', t);
		EXECUTE format('CREATE POLICY org_isolation ON wms_bridge.%I USING (org_id = wms_bridge.current_org()) WITH CHECK (org_id = wms_bridge.current_org())', t);
	END LOOP;
END $$;

GRANT USAGE ON SCHEMA wms_bridge TO kgarden_mes_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA wms_bridge TO kgarden_mes_app;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA wms_bridge TO kgarden_mes_app;
