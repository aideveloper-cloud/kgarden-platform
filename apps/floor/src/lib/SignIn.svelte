<script lang="ts">
	// Sign-in for the floor tablet. Who may use it, and as what, is set in ERPNext (K Garden: app
	// access). On a shared tablet people use their employee code + PIN; on a personal device
	// "เข้าสู่ระบบด้วย K Garden" goes through the ERPNext sign-in page.
	import Keypad from '#lib/Keypad.svelte';
	import { auth, type Me, MesError } from '#lib/mes.js';

	let { onSignedIn }: { onSignedIn: (me: Me) => void } = $props();

	let code = $state('');
	let pin = $state('');
	let busy = $state(false);
	let error = $state('');

	async function signIn() {
		busy = true;
		error = '';
		try {
			onSignedIn(await auth.pin(code.trim(), pin));
		} catch (e) {
			error = (e as MesError).message;
			pin = '';
		} finally {
			busy = false;
		}
	}

	async function sso() {
		busy = true;
		error = '';
		try {
			window.location.href = (await auth.ssoStart()).url;
		} catch (e) {
			error = (e as MesError).message;
			busy = false;
		}
	}
</script>

<main class="flex min-h-dvh items-center justify-center p-4">
	<div class="w-full max-w-sm space-y-4 rounded-2xl bg-white p-6 shadow">
		<h1 class="text-2xl font-bold text-brand-700">K Garden · หน้างานผลิต</h1>
		<form
			class="space-y-3"
			onsubmit={(e) => {
				e.preventDefault();
				signIn();
			}}
		>
			<label class="block space-y-1">
				<span class="text-stone-600">รหัสพนักงาน</span>
				<input
					bind:value={code}
					class="h-14 w-full rounded-xl border border-stone-300 px-4 text-xl uppercase"
					autocomplete="off"
					autocapitalize="characters"
					data-testid="employee-code"
				/>
			</label>
			<div>
				<span class="text-stone-600">PIN</span>
				<p class="my-2 h-10 text-center text-3xl tracking-[0.5em]" data-testid="pin-dots">
					{'●'.repeat(pin.length) || ' '}
				</p>
				<Keypad bind:value={pin} disabled={busy} digitsOnly />
			</div>
			{#if error}
				<p class="rounded-lg bg-red-50 p-3 text-red-700" role="alert">{error}</p>
			{/if}
			<button
				disabled={busy || !code.trim() || pin.length < 4}
				class="h-14 w-full rounded-xl bg-brand-600 text-xl font-semibold text-white disabled:opacity-40"
			>
				{busy ? 'กำลังตรวจสอบ…' : 'เข้าสู่ระบบ'}
			</button>
		</form>
		<div class="flex items-center gap-3 text-sm text-stone-400"><span class="h-px flex-1 bg-stone-200"></span>หรือ<span class="h-px flex-1 bg-stone-200"></span></div>
		<button
			onclick={sso}
			disabled={busy}
			class="h-12 w-full rounded-xl border border-brand-600 font-semibold text-brand-700 disabled:opacity-40"
		>
			เข้าสู่ระบบด้วย K Garden
		</button>
		<p class="text-center text-xs text-stone-500">สิทธิ์ใช้งานจัดการโดยผู้ดูแลระบบใน ERPNext</p>
	</div>
</main>
