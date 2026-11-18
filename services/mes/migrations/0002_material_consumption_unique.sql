-- A resent outbox event (worker crashed after ERPNext accepted the Stock Entry) re-copies the
-- consumed materials. Make that copy idempotent: one row per output, item and source warehouse.

DELETE FROM mes.material_consumption a
USING mes.material_consumption b
WHERE a.output_id = b.output_id
	AND a.item_code = b.item_code
	AND a.source_warehouse = b.source_warehouse
	AND (a.created_at, a.id) > (b.created_at, b.id);

ALTER TABLE mes.material_consumption
	ADD CONSTRAINT material_consumption_one_per_item UNIQUE (output_id, item_code, source_warehouse);
