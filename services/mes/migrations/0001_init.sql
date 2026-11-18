-- M1: MES (production shop-floor) data model.
-- ERPNext stays the owner of Work Order, BOM, Item and the stock ledger; this schema only holds
-- what the shop floor adds (operation logs, reported output, scrap) plus a read-only mirror of
-- Work Orders and the outbox that posts results back to ERPNext.
--
-- Rules (from the plan): quantities are numeric, every table has org_id + RLS,
-- created_at / updated_at / version on mutable tables, audit_log is append-only.

CREATE SCHEMA mes;

CREATE FUNCTION mes.touch_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	NEW.updated_at := now();
	NEW.version := OLD.version + 1;
	RETURN NEW;
END $$;

CREATE FUNCTION mes.current_org() RETURNS uuid LANGUAGE sql STABLE AS $$
	SELECT nullif(current_setting('app.org_id', true), '')::uuid
$$;

CREATE TABLE mes.work_center (
	id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	org_id     uuid NOT NULL,
	code       text NOT NULL,
	name       text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	version    integer NOT NULL DEFAULT 1,
	UNIQUE (org_id, code)
);

-- Read-only copy of submitted ERPNext Work Orders (M5). ERPNext wins on any difference.
CREATE TABLE mes.mirror_work_order (
	org_id           uuid NOT NULL,
	name             text NOT NULL,
	production_item  text NOT NULL,
	item_name        text NOT NULL DEFAULT '',
	bom_no           text NOT NULL DEFAULT '',
	qty              numeric(21, 9) NOT NULL,
	produced_qty     numeric(21, 9) NOT NULL DEFAULT 0,
	stock_uom        text NOT NULL DEFAULT '',
	status           text NOT NULL,
	erpnext_modified timestamptz,
	synced_at        timestamptz NOT NULL DEFAULT now(),
	created_at       timestamptz NOT NULL DEFAULT now(),
	updated_at       timestamptz NOT NULL DEFAULT now(),
	version          integer NOT NULL DEFAULT 1,
	PRIMARY KEY (org_id, name)
);

CREATE TABLE mes.operation_log (
	id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	org_id         uuid NOT NULL,
	work_order     text NOT NULL,
	work_center_id uuid REFERENCES mes.work_center (id),
	operator       text NOT NULL,
	started_at     timestamptz NOT NULL DEFAULT now(),
	ended_at       timestamptz,
	note           text NOT NULL DEFAULT '',
	created_at     timestamptz NOT NULL DEFAULT now(),
	updated_at     timestamptz NOT NULL DEFAULT now(),
	version        integer NOT NULL DEFAULT 1,
	CHECK (ended_at IS NULL OR ended_at >= started_at)
);
-- At most one running operation per Work Order.
CREATE UNIQUE INDEX operation_log_one_open ON mes.operation_log (org_id, work_order) WHERE ended_at IS NULL;

-- Good output reported from the floor. client_key makes a retried tablet request a no-op.
CREATE TABLE mes.production_output (
	id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	org_id      uuid NOT NULL,
	work_order  text NOT NULL,
	good_qty    numeric(21, 9) NOT NULL CHECK (good_qty >= 0),
	scrap_qty   numeric(21, 9) NOT NULL DEFAULT 0 CHECK (scrap_qty >= 0),
	operator    text NOT NULL,
	client_key  text NOT NULL,
	status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'posted', 'failed')),
	stock_entry text,
	created_at  timestamptz NOT NULL DEFAULT now(),
	updated_at  timestamptz NOT NULL DEFAULT now(),
	version     integer NOT NULL DEFAULT 1,
	UNIQUE (org_id, client_key),
	CHECK (good_qty > 0 OR scrap_qty > 0)
);
CREATE INDEX production_output_wo ON mes.production_output (org_id, work_order);

-- Raw materials ERPNext actually consumed for an output (copied from the posted Stock Entry).
CREATE TABLE mes.material_consumption (
	id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	org_id           uuid NOT NULL,
	output_id        uuid NOT NULL REFERENCES mes.production_output (id),
	item_code        text NOT NULL,
	qty              numeric(21, 9) NOT NULL,
	stock_uom        text NOT NULL,
	source_warehouse text NOT NULL DEFAULT '',
	created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mes.scrap_log (
	id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	org_id     uuid NOT NULL,
	output_id  uuid NOT NULL REFERENCES mes.production_output (id),
	qty        numeric(21, 9) NOT NULL CHECK (qty > 0),
	reason     text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);

-- Transactional outbox: written in the same transaction as the business row, sent by the worker.
CREATE TABLE mes.outbox_event (
	id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	org_id          uuid NOT NULL,
	event_type      text NOT NULL,
	aggregate_id    uuid NOT NULL,
	idempotency_key text NOT NULL,
	payload         jsonb NOT NULL,
	status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'dead')),
	attempts        integer NOT NULL DEFAULT 0,
	next_attempt_at timestamptz NOT NULL DEFAULT now(),
	last_error      text NOT NULL DEFAULT '',
	result          jsonb,
	created_at      timestamptz NOT NULL DEFAULT now(),
	updated_at      timestamptz NOT NULL DEFAULT now(),
	version         integer NOT NULL DEFAULT 1,
	UNIQUE (org_id, idempotency_key)
);
CREATE INDEX outbox_event_due ON mes.outbox_event (next_attempt_at) WHERE status = 'pending';

-- Inbound event ids already processed (ERPNext webhooks), so redelivery is harmless.
CREATE TABLE mes.inbox_event (
	org_id      uuid NOT NULL,
	event_id    text NOT NULL,
	source      text NOT NULL,
	received_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (org_id, event_id)
);

CREATE TABLE mes.audit_log (
	id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	org_id    uuid NOT NULL,
	actor     text NOT NULL,
	action    text NOT NULL,
	entity    text NOT NULL,
	entity_id text NOT NULL,
	data      jsonb NOT NULL DEFAULT '{}',
	at        timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION mes.audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	RAISE EXCEPTION 'audit_log is append-only';
END $$;
CREATE TRIGGER audit_log_no_change BEFORE UPDATE OR DELETE ON mes.audit_log
	FOR EACH ROW EXECUTE FUNCTION mes.audit_log_immutable();

DO $$
DECLARE t text;
BEGIN
	FOREACH t IN ARRAY ARRAY['work_center', 'mirror_work_order', 'operation_log', 'production_output', 'outbox_event'] LOOP
		EXECUTE format('CREATE TRIGGER touch BEFORE UPDATE ON mes.%I FOR EACH ROW EXECUTE FUNCTION mes.touch_row()', t);
	END LOOP;

	FOREACH t IN ARRAY ARRAY['work_center', 'mirror_work_order', 'operation_log', 'production_output',
		'material_consumption', 'scrap_log', 'outbox_event', 'inbox_event', 'audit_log'] LOOP
		EXECUTE format('ALTER TABLE mes.%I ENABLE ROW LEVEL SECURITY', t);
		EXECUTE format('CREATE POLICY org_isolation ON mes.%I USING (org_id = mes.current_org()) WITH CHECK (org_id = mes.current_org())', t);
	END LOOP;
END $$;

-- The service connects as kgarden_mes_app: not the owner, so RLS applies, and it can never delete.
GRANT USAGE ON SCHEMA mes TO kgarden_mes_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA mes TO kgarden_mes_app;
REVOKE UPDATE ON mes.audit_log, mes.inbox_event FROM kgarden_mes_app;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA mes TO kgarden_mes_app;
