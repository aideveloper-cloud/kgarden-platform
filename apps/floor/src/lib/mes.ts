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
	if (!res.ok) throw new MesError(res.status, data.error ?? `HTTP ${res.status}`);
	return data as T;
}

export const mes = {
	workOrders: () => call<{ data: WorkOrder[] | null }>('GET', '/work-orders').then((r) => r.data ?? []),
	workOrder: (name: string) => call<WorkOrderDetail>('GET', `/work-orders/${encodeURIComponent(name)}`),
	sync: () => call<{ synced: number }>('POST', '/sync/work-orders'),
	start: (name: string, operator: string) => call('POST', `/work-orders/${encodeURIComponent(name)}/start`, { operator }),
	stop: (name: string, operator: string) => call('POST', `/work-orders/${encodeURIComponent(name)}/stop`, { operator }),
	// key must stay the same when the operator retries the same report.
	output: (name: string, key: string, body: { good_qty: string; scrap_qty: string; scrap_reason: string; operator: string }) =>
		call<{ output_id: string; status: string; replayed: boolean }>(
			'POST',
			`/work-orders/${encodeURIComponent(name)}/output`,
			body,
			{ 'Idempotency-Key': key }
		)
};

export const fmt = (n: string | number) => Number(n).toLocaleString('th-TH', { maximumFractionDigits: 3 });
