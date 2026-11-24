-- Production POS (production-pos /pos/production, 2026-11-24): finished goods reported there are
-- WMS receipts with reference_type 'production_output' and the ERPNext Work Order in the ledger
-- row's meta. The bridge posts them as Manufacture Stock Entries against that Work Order.
ALTER TABLE wms_bridge.ledger_event ADD COLUMN work_order text NOT NULL DEFAULT '';
