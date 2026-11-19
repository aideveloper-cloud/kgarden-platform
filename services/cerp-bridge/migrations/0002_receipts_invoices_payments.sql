-- Goods receipts, supplier invoices and payments from ConstructionERP, booked in ERPNext as
-- Purchase Receipt, Purchase Invoice and Payment Entry (with Thai WHT as a deduction).
--
-- One row per ConstructionERP document: what was read and the ERPNext document that stands for it.
-- live = it must exist in ERPNext (receipt confirmed; invoice approved/sent/paid; every payment).

CREATE TABLE cerp_bridge.doc_state (
	org_id          uuid NOT NULL,
	kind            text NOT NULL CHECK (kind IN ('receipt', 'invoice', 'payment')),
	cerp_id         uuid NOT NULL,
	ref             text NOT NULL,           -- the number people know it by
	cerp_status     text NOT NULL,
	live            boolean NOT NULL,
	content_hash    text NOT NULL,
	payload         jsonb NOT NULL,
	cerp_updated_at timestamptz NOT NULL,
	erpnext_doc     text,
	sync_status     text NOT NULL DEFAULT 'pending' CHECK (sync_status IN ('pending', 'synced', 'dead')),
	attempts        int NOT NULL DEFAULT 0,
	next_attempt_at timestamptz NOT NULL DEFAULT now(),
	last_error      text NOT NULL DEFAULT '',
	synced_at       timestamptz,
	PRIMARY KEY (org_id, kind, cerp_id)
);
CREATE INDEX doc_state_due ON cerp_bridge.doc_state (sync_status, next_attempt_at);

CREATE TABLE cerp_bridge.kind_cursor (
	org_id        uuid NOT NULL,
	kind          text NOT NULL,
	updated_after timestamptz NOT NULL DEFAULT 'epoch',
	PRIMARY KEY (org_id, kind)
);

ALTER TABLE cerp_bridge.audit_log ALTER COLUMN cerp_po_id DROP NOT NULL;
ALTER TABLE cerp_bridge.audit_log ADD COLUMN kind text NOT NULL DEFAULT 'po';
ALTER TABLE cerp_bridge.audit_log ADD COLUMN cerp_id uuid;

DO $$
DECLARE t text;
BEGIN
	FOREACH t IN ARRAY ARRAY['doc_state', 'kind_cursor'] LOOP
		EXECUTE format('ALTER TABLE cerp_bridge.%I ENABLE ROW LEVEL SECURITY', t);
		EXECUTE format('CREATE POLICY org_isolation ON cerp_bridge.%I USING (org_id = cerp_bridge.current_org()) WITH CHECK (org_id = cerp_bridge.current_org())', t);
	END LOOP;
END $$;

GRANT SELECT, INSERT, UPDATE ON cerp_bridge.doc_state, cerp_bridge.kind_cursor TO kgarden_mes_app;
