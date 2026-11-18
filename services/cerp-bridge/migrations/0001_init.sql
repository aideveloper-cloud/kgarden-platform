-- ConstructionERP -> ERPNext bridge. ConstructionERP keeps purchasing (PR -> PO -> approval);
-- ERPNext books approved purchase orders in the company KGF Construction.
--
-- One row per ConstructionERP PO: the last version read (payload + content hash) and the ERPNext
-- Purchase Order that currently stands for it. A PO that is live (approved/issued/received) must
-- have a submitted ERPNext PO with the same content; one that is draft/cancelled must have none.

CREATE SCHEMA cerp_bridge;

CREATE TABLE cerp_bridge.po_state (
	org_id          uuid NOT NULL,
	cerp_po_id      uuid NOT NULL,
	po_number       text NOT NULL,
	cerp_status     text NOT NULL,
	live            boolean NOT NULL,
	content_hash    text NOT NULL,
	payload         jsonb NOT NULL,
	cerp_updated_at timestamptz NOT NULL,
	erpnext_po      text,             -- submitted ERPNext PO standing for this version, if any
	erpnext_key     text,             -- its kg_idempotency_key: cerp-po-<id>-<hash>
	sync_status     text NOT NULL DEFAULT 'pending' CHECK (sync_status IN ('pending', 'synced', 'dead')),
	attempts        int NOT NULL DEFAULT 0,
	next_attempt_at timestamptz NOT NULL DEFAULT now(),
	last_error      text NOT NULL DEFAULT '',
	synced_at       timestamptz,
	PRIMARY KEY (org_id, cerp_po_id)
);
CREATE INDEX po_state_due ON cerp_bridge.po_state (sync_status, next_attempt_at);

CREATE TABLE cerp_bridge.read_cursor (
	org_id        uuid PRIMARY KEY,
	updated_after timestamptz NOT NULL DEFAULT 'epoch'
);

-- What the bridge did in ERPNext, append-only.
CREATE TABLE cerp_bridge.audit_log (
	id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	org_id     uuid NOT NULL,
	cerp_po_id uuid NOT NULL,
	action     text NOT NULL,
	erpnext_po text NOT NULL DEFAULT '',
	detail     text NOT NULL DEFAULT '',
	at         timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION cerp_bridge.current_org() RETURNS uuid
LANGUAGE sql STABLE AS $$ SELECT nullif(current_setting('app.org_id', true), '')::uuid $$;

DO $$
DECLARE t text;
BEGIN
	FOREACH t IN ARRAY ARRAY['po_state', 'read_cursor', 'audit_log'] LOOP
		EXECUTE format('ALTER TABLE cerp_bridge.%I ENABLE ROW LEVEL SECURITY', t);
		EXECUTE format('CREATE POLICY org_isolation ON cerp_bridge.%I USING (org_id = cerp_bridge.current_org()) WITH CHECK (org_id = cerp_bridge.current_org())', t);
	END LOOP;
END $$;

GRANT USAGE ON SCHEMA cerp_bridge TO kgarden_mes_app;
GRANT SELECT, INSERT, UPDATE ON cerp_bridge.po_state, cerp_bridge.read_cursor TO kgarden_mes_app;
GRANT SELECT, INSERT ON cerp_bridge.audit_log TO kgarden_mes_app;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA cerp_bridge TO kgarden_mes_app;
