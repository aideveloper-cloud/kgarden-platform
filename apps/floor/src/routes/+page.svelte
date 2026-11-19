<script lang="ts">
	import { onMount } from 'svelte';
	import Keypad from '#lib/Keypad.svelte';
	import SignIn from '#lib/SignIn.svelte';
	import { auth, fmt, mes, MesError, whenSignedOut, type Me, type WorkOrder, type WorkOrderDetail } from '#lib/mes.js';

	const SCRAP_REASONS = ['ไม้แตก', 'สีไม่สม่ำเสมอ', 'ขนาดไม่ได้', 'วัสดุเสีย'];

	// Who is signed in (from ERPNext-managed access); null = show the sign-in screen.
	let me = $state<Me | null>(null);
	let checking = $state(true);
	// Shared tablet: sign out after this long without a touch.
	const IDLE_MS = 10 * 60 * 1000;
	let orders = $state<WorkOrder[]>([]);
	let selected = $state<string | null>(null);
	let detail = $state<WorkOrderDetail | null>(null);
	let loadError = $state('');
	let busy = $state(false);
	let toast = $state<{ kind: 'ok' | 'err'; text: string } | null>(null);

	// Output form. `pendingKey` survives a failed send so the retry is the same request (no double count).
	let field = $state<'good' | 'scrap'>('good');
	let good = $state('0');
	let scrap = $state('0');
	let scrapReason = $state('');
	let pendingKey = $state<string | null>(null);

	let current = $derived(orders.find((o) => o.name === selected) ?? null);
	let remaining = $derived(current ? Number(current.qty) - Number(current.produced_qty) - Number(current.pending_qty) : 0);
	let canSend = $derived(
		!busy && (Number(good) > 0 || Number(scrap) > 0) && Number(good) <= remaining && (Number(scrap) === 0 || scrapReason.trim() !== '')
	);

	onMount(() => {
		whenSignedOut(() => (me = null));
		auth
			.me()
			.then((m) => {
				me = m;
				refresh(); // signed in already (or back from ERPNext): show the work orders at once
			})
			.catch(() => (me = null))
			.finally(() => (checking = false));
		const t = setInterval(() => me && refresh(), 3000);
		let idle = setTimeout(signOut, IDLE_MS);
		const touch = () => {
			clearTimeout(idle);
			idle = setTimeout(() => me && signOut(), IDLE_MS);
		};
		for (const ev of ['pointerdown', 'keydown']) window.addEventListener(ev, touch);
		return () => {
			clearInterval(t);
			clearTimeout(idle);
			for (const ev of ['pointerdown', 'keydown']) window.removeEventListener(ev, touch);
		};
	});

	async function signOut() {
		await auth.logout().catch(() => {});
		me = null;
		selected = null;
		detail = null;
	}

	async function refresh() {
		try {
			orders = await mes.workOrders();
			if (selected) detail = await mes.workOrder(selected);
			loadError = '';
		} catch (e) {
			loadError = (e as Error).message;
		}
	}

	function say(kind: 'ok' | 'err', text: string) {
		toast = { kind, text };
		setTimeout(() => (toast = null), 4000);
	}

	function resetForm() {
		good = '0';
		scrap = '0';
		scrapReason = '';
		field = 'good';
		pendingKey = null;
	}

	async function open(name: string) {
		if (pendingKey) return say('err', 'มีรายการที่ยังส่งไม่สำเร็จ กดส่งซ้ำหรือยกเลิกก่อน');
		selected = name;
		resetForm();
		detail = null;
		await refresh();
		// Narrow screens stack the report under the list: bring it into view.
		if (!matchMedia('(min-width: 768px)').matches) {
			document.getElementById('report')?.scrollIntoView({ block: 'start' });
		}
	}

	async function run(fn: () => Promise<unknown>, okText: string) {
		busy = true;
		try {
			await fn();
			say('ok', okText);
			await refresh();
		} catch (e) {
			say('err', (e as Error).message);
		} finally {
			busy = false;
		}
	}

	async function send() {
		if (!current) return;
		pendingKey ??= crypto.randomUUID();
		busy = true;
		try {
			const r = await mes.output(current.name, pendingKey, {
				good_qty: good,
				scrap_qty: scrap,
				scrap_reason: scrapReason
			});
			say('ok', r.replayed ? 'บันทึกไว้แล้วก่อนหน้านี้ (ไม่นับซ้ำ)' : `บันทึกแล้ว ดี ${fmt(good)} เสีย ${fmt(scrap)}`);
			resetForm();
			await refresh();
		} catch (e) {
			const err = e as MesError;
			// Network error or server error: keep the key and values for an exact retry.
			if (err.status === 0 || err.status >= 500) say('err', `${err.message} — กด "ส่งซ้ำ" ระบบจะไม่นับซ้ำ`);
			else {
				pendingKey = null;
				say('err', err.message);
			}
		} finally {
			busy = false;
		}
	}

	const roleText: Record<string, string> = { operator: 'ผู้ปฏิบัติงาน', supervisor: 'หัวหน้างาน', superadmin: 'ผู้ดูแลระบบ' };
	const statusText = { pending: 'รอส่ง ERP', posted: 'ส่งแล้ว', failed: 'ส่งไม่สำเร็จ' } as const;
	const statusClass = {
		pending: 'bg-amber-100 text-amber-800',
		posted: 'bg-brand-100 text-brand-700',
		failed: 'bg-red-100 text-red-700'
	} as const;
	const time = (s: string) => new Date(s).toLocaleTimeString('th-TH', { hour: '2-digit', minute: '2-digit' });
</script>

{#if checking}
	<main class="flex min-h-dvh items-center justify-center text-stone-500">กำลังโหลด…</main>
{:else if !me}
	<SignIn
		onSignedIn={(m) => {
			me = m;
			refresh();
		}}
	/>
{:else}
	<div class="flex min-h-dvh flex-col">
		<header class="flex items-center justify-between gap-3 bg-brand-700 px-4 py-3 text-white">
			<h1 class="text-lg font-bold">K Garden · หน้างานผลิต</h1>
			<div class="flex items-center gap-2">
				{#if me.role !== 'operator'}
					<button class="rounded-lg bg-white/15 px-3 py-2" onclick={() => run(() => mes.sync(), 'ดึงใบสั่งผลิตล่าสุดแล้ว')} disabled={busy}>
						รีเฟรช
					</button>
				{/if}
				<span class="rounded-lg bg-white/15 px-3 py-2" data-testid="signed-in">
					👤 {me.full_name}{me.employee_code ? ` (${me.employee_code})` : ''} · {roleText[me.role] ?? me.role}
				</span>
				<button class="rounded-lg bg-white px-3 py-2 font-semibold text-brand-700" onclick={signOut}>ออกจากระบบ</button>
			</div>
		</header>

		{#if loadError}
			<p class="bg-red-600 px-4 py-2 text-white">{loadError}</p>
		{/if}

		<div class="grid flex-1 gap-4 p-4 md:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
			<!-- Work orders -->
			<section class="space-y-2" aria-label="ใบสั่งผลิต">
				<h2 class="font-semibold text-stone-600">ใบสั่งผลิตที่เปิดอยู่ ({orders.length})</h2>
				{#each orders as o (o.name)}
					{@const pct = Math.min(100, ((Number(o.produced_qty) + Number(o.pending_qty)) / Number(o.qty)) * 100)}
					<button
						onclick={() => open(o.name)}
						class={[
							'block w-full rounded-2xl bg-white p-4 text-left shadow-sm ring-2',
							o.name === selected ? 'ring-brand-500' : 'ring-transparent'
						]}
					>
						<div class="flex items-start justify-between gap-2">
							<div class="min-w-0">
								<p class="truncate text-lg font-semibold">{o.item_name}</p>
								<p class="text-sm text-stone-500">{o.name}</p>
							</div>
							{#if o.running}
								<span class="shrink-0 rounded-full bg-brand-100 px-2 py-1 text-sm font-semibold text-brand-700">● กำลังผลิต</span>
							{/if}
						</div>
						<div class="mt-3 h-3 overflow-hidden rounded-full bg-stone-200">
							<div class="h-full bg-brand-500" style:width="{pct}%"></div>
						</div>
						<p class="mt-1 text-sm text-stone-600">
							ผลิตแล้ว {fmt(o.produced_qty)}{#if Number(o.pending_qty) > 0}&nbsp;(+{fmt(o.pending_qty)} รอส่ง){/if} / {fmt(o.qty)}
							{o.stock_uom}
						</p>
					</button>
				{:else}
					<p class="rounded-2xl bg-white p-6 text-center text-stone-500">ไม่มีใบสั่งผลิตที่เปิดอยู่</p>
				{/each}
			</section>

			<!-- Selected work order -->
			<section id="report" class="scroll-mt-4 rounded-2xl bg-white p-4 shadow-sm" aria-label="รายงานผล">
				{#if !current}
					<p class="py-16 text-center text-stone-500">เลือกใบสั่งผลิตจากรายการ</p>
				{:else}
					<div class="flex flex-wrap items-center justify-between gap-3">
						<div>
							<h2 class="text-xl font-bold">{current.item_name}</h2>
							<p class="text-stone-500">{current.name} · เหลือ {fmt(remaining)} {current.stock_uom}</p>
						</div>
						{#if current.running}
							<button
								disabled={busy}
								onclick={() => run(() => mes.stop(current!.name), 'หยุดงานแล้ว')}
								class="h-14 rounded-xl bg-stone-800 px-6 text-lg font-semibold text-white disabled:opacity-40">■ หยุดงาน</button
							>
						{:else}
							<button
								disabled={busy}
								onclick={() => run(() => mes.start(current!.name), 'เริ่มงานแล้ว')}
								class="h-14 rounded-xl bg-brand-600 px-6 text-lg font-semibold text-white disabled:opacity-40">▶ เริ่มงาน</button
							>
						{/if}
					</div>

					<div class="mt-4 grid gap-4 lg:grid-cols-2">
						<div class="space-y-3">
							<div class="grid grid-cols-2 gap-2">
								<button
									onclick={() => (field = 'good')}
									class={['rounded-xl p-3 text-left ring-2', field === 'good' ? 'bg-brand-50 ring-brand-500' : 'bg-stone-50 ring-stone-200']}
								>
									<span class="text-sm text-stone-600">ของดี</span>
									<span class={['block text-3xl font-bold', Number(good) > remaining && 'text-red-600']}>{good}</span>
								</button>
								<button
									onclick={() => (field = 'scrap')}
									class={['rounded-xl p-3 text-left ring-2', field === 'scrap' ? 'bg-red-50 ring-red-400' : 'bg-stone-50 ring-stone-200']}
								>
									<span class="text-sm text-stone-600">ของเสีย</span>
									<span class="block text-3xl font-bold">{scrap}</span>
								</button>
							</div>
							{#if Number(good) > remaining}
								<p class="text-red-600">เกินยอดที่เหลือ ({fmt(remaining)})</p>
							{/if}
							{#if field === 'good'}
								<Keypad bind:value={good} disabled={!!pendingKey} />
							{:else}
								<Keypad bind:value={scrap} disabled={!!pendingKey} />
							{/if}
						</div>

						<div class="flex flex-col gap-3">
							{#if Number(scrap) > 0}
								<fieldset class="space-y-2">
									<legend class="text-sm text-stone-600">สาเหตุของเสีย</legend>
									<div class="flex flex-wrap gap-2">
										{#each SCRAP_REASONS as r (r)}
											<button
												disabled={!!pendingKey}
												onclick={() => (scrapReason = r)}
												class={['rounded-full px-4 py-2 ring-1', scrapReason === r ? 'bg-red-600 text-white ring-red-600' : 'ring-stone-300']}
												>{r}</button
											>
										{/each}
									</div>
									<input
										bind:value={scrapReason}
										disabled={!!pendingKey}
										placeholder="หรือพิมพ์สาเหตุ"
										class="h-12 w-full rounded-xl border border-stone-300 px-3"
									/>
								</fieldset>
							{/if}

							<button
								disabled={!canSend}
								onclick={send}
								class="mt-auto h-16 rounded-xl bg-brand-600 text-xl font-bold text-white disabled:opacity-40"
							>
								{busy ? 'กำลังส่ง…' : pendingKey ? 'ส่งซ้ำ' : 'บันทึกผลผลิต'}
							</button>
							{#if pendingKey && !busy}
								<button class="text-stone-500 underline" onclick={resetForm}>
									ยกเลิก (ตรวจในประวัติด้านล่างก่อนว่ารายการเข้าแล้วหรือยัง)
								</button>
							{/if}
						</div>
					</div>

					<h3 class="mt-6 mb-2 font-semibold text-stone-600">ประวัติการรายงาน</h3>
					<ul class="divide-y divide-stone-100">
						{#each [...(detail?.outputs ?? [])].reverse() as out (out.id)}
							<li class="flex flex-wrap items-center justify-between gap-2 py-2">
								<span>
									<span class="text-stone-500">{time(out.created_at)}</span>
									ดี <b>{fmt(out.good_qty)}</b>
									{#if Number(out.scrap_qty) > 0}· เสีย <b>{fmt(out.scrap_qty)}</b>{/if}
									<span class="text-stone-500">· {out.operator}</span>
								</span>
								<span class="flex items-center gap-2">
									{#if out.stock_entry}<span class="text-sm text-stone-500">{out.stock_entry}</span>{/if}
									<span class={['rounded-full px-2 py-0.5 text-sm', statusClass[out.status]]}
										>{out.status === 'pending' && out.waiting_reason.includes('NegativeStockError')
											? 'รอวัตถุดิบเข้า WIP'
											: statusText[out.status]}</span
									>
								</span>
							</li>
						{:else}
							<li class="py-2 text-stone-500">ยังไม่มีการรายงาน</li>
						{/each}
					</ul>
				{/if}
			</section>
		</div>
	</div>

	{#if toast}
		<div
			role="status"
			class={[
				'fixed inset-x-4 bottom-4 mx-auto max-w-md rounded-xl px-4 py-3 text-center text-lg text-white shadow-lg',
				toast.kind === 'ok' ? 'bg-brand-700' : 'bg-red-600'
			]}
		>
			{toast.text}
		</div>
	{/if}
{/if}
