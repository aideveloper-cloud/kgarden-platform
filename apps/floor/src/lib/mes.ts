// Client for the MES service (proxied at /api). Quantities stay strings, as the API sends them,
// so nothing is rounded through floating point on the way.

export type WorkOrder = {
	name: string;
	production_item: string;
	item_name: string;
	qty: string;
	produced_qty: string;
	pending_qty: string;
	stock_uom: string;
	status: string;
	running: boolean;
};

export type Output = {
	id: string;
	good_qty: string;
	scrap_qty: string;
	operator: string;
	status: 'pending' | 'posted' | 'failed';
	stock_entry: string;
	created_at: string;
	waiting_reason: string;
};

export type WorkOrderDetail = Omit<WorkOrder, 'pending_qty' | 'running'> & {
	outputs: Output[];
	operation_logs: { id: string; operator: string; started_at: string; ended_at: string | null }[];
};

export class MesError extends Error {
	constructor(
		public status: number,
		message: string
	) {
		super(message);
	}
}

export type Me = { user: string; full_name: string; employee_code: string; role: string; method: 'pin' | 'sso' };

/** Called when the MES says the session is gone (expired, logged out, access removed in ERPNext). */
let onSignedOut: () => void = () => {};
export function whenSignedOut(fn: () => void) {
	onSignedOut = fn;
}

async function call<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
	let res: Response;
	try {
		res = await fetch(`/api${path}`, {
			method,
			headers: { 'Content-Type': 'application/json', ...headers },
			body: body === undefined ? undefined : JSON.stringify(body)
		});
	} catch {
		throw new MesError(0, 'เชื่อมต่อระบบไม่ได้ ตรวจสอบเครือข่ายแล้วลองอีกครั้ง');
	}
	const data = await res.json().catch(() => ({}));
	if (res.status === 401 && !path.startsWith('/auth/')) onSignedOut();
	if (!res.ok) throw new MesError(res.status, data.error ?? `HTTP ${res.status}`);
	return data as T;
}

export const mes = {
	workOrders: () => call<{ data: WorkOrder[] | null }>('GET', '/work-orders').then((r) => r.data ?? []),
	workOrder: (name: string) => call<WorkOrderDetail>('GET', `/work-orders/${encodeURIComponent(name)}`),
	sync: () => call<{ synced: number }>('POST', '/sync/work-orders'),
	// Who did it comes from the signed-in session, not from the request.
	start: (name: string) => call('POST', `/work-orders/${encodeURIComponent(name)}/start`, {}),
	stop: (name: string) => call('POST', `/work-orders/${encodeURIComponent(name)}/stop`, {}),
	// key must stay the same when the operator retries the same report.
	output: (name: string, key: string, body: { good_qty: string; scrap_qty: string; scrap_reason: string }) =>
		call<{ output_id: string; status: string; replayed: boolean }>(
			'POST',
			`/work-orders/${encodeURIComponent(name)}/output`,
			body,
			{ 'Idempotency-Key': key }
		)
};

/** Sign-in: people and their MES access are managed in ERPNext. */
export const auth = {
	me: () => call<Me>('GET', '/auth/me'),
	pin: (employee_code: string, pin: string) => call<Me>('POST', '/auth/pin', { employee_code, pin }),
	ssoStart: () => call<{ url: string }>('GET', '/auth/oidc/start'),
	ssoCallback: (code: string, state: string) => call<Me>('POST', '/auth/oidc/callback', { code, state }),
	logout: () => call('POST', '/auth/logout')
};

export const fmt = (n: string | number) => Number(n).toLocaleString('th-TH', { maximumFractionDigits: 3 });
