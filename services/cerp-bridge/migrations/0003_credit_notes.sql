-- Credit notes: a ConstructionERP invoice reversed after payment is booked in ERPNext as a Debit Note
-- (return of the whole Purchase Invoice). One row per reversed invoice; cerp_id is the invoice id.

ALTER TABLE cerp_bridge.doc_state DROP CONSTRAINT doc_state_kind_check;
ALTER TABLE cerp_bridge.doc_state ADD CONSTRAINT doc_state_kind_check
	CHECK (kind IN ('receipt', 'invoice', 'credit_note', 'payment'));
