-- Who is using the floor tablet. People sign in through ERPNext (the core): employee code + PIN on a
-- shared tablet, or "เข้าสู่ระบบด้วย K Garden" (OpenID Connect). The session token is stored hashed.
-- A session ends when it expires, on logout, or when ERPNext reports the person lost MES access.

CREATE TABLE mes.session (
	id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	org_id        uuid NOT NULL,
	token_hash    text NOT NULL UNIQUE,
	user_id       text NOT NULL,           -- ERPNext user (email)
	full_name     text NOT NULL DEFAULT '',
	employee_code text NOT NULL DEFAULT '',
	role          text NOT NULL,           -- operator | supervisor | superadmin (from ERPNext)
	method        text NOT NULL CHECK (method IN ('pin', 'sso')),
	created_at    timestamptz NOT NULL DEFAULT now(),
	expires_at    timestamptz NOT NULL,
	revoked_at    timestamptz,
	revoke_reason text NOT NULL DEFAULT ''
);
CREATE INDEX session_open_by_user ON mes.session (user_id) WHERE revoked_at IS NULL;

-- OpenID Connect sign-in in progress (state parameter, single use, short-lived).
CREATE TABLE mes.oidc_state (
	state      text PRIMARY KEY,
	org_id     uuid NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);

-- How far ERPNext access changes have been read (ERPNext's own local timestamps).
CREATE TABLE mes.access_cursor (
	org_id        uuid PRIMARY KEY,
	checked_since text NOT NULL
);

DO $$
DECLARE t text;
BEGIN
	FOREACH t IN ARRAY ARRAY['session', 'oidc_state', 'access_cursor'] LOOP
		EXECUTE format('ALTER TABLE mes.%I ENABLE ROW LEVEL SECURITY', t);
		EXECUTE format('CREATE POLICY org_isolation ON mes.%I USING (org_id = mes.current_org()) WITH CHECK (org_id = mes.current_org())', t);
	END LOOP;
END $$;

GRANT SELECT, INSERT, UPDATE ON mes.session, mes.access_cursor TO kgarden_mes_app;
GRANT SELECT, INSERT, DELETE ON mes.oidc_state TO kgarden_mes_app;
