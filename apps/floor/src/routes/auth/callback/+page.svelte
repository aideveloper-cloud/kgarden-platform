<script lang="ts">
	// ERPNext sends the browser back here after "เข้าสู่ระบบด้วย K Garden"; the MES exchanges the
	// code for the person's identity and MES role (server side), then the tablet opens.
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { auth, MesError } from '#lib/mes.js';

	let error = $state('');

	onMount(async () => {
		const q = new URLSearchParams(window.location.search);
		if (q.get('error')) {
			error = 'ERPNext ไม่อนุญาตให้เข้าสู่ระบบ';
			return;
		}
		try {
			await auth.ssoCallback(q.get('code') ?? '', q.get('state') ?? '');
			await goto('/', { replaceState: true });
		} catch (e) {
			error = (e as MesError).message;
		}
	});
</script>

<main class="flex min-h-dvh items-center justify-center p-4">
	{#if error}
		<div class="max-w-sm space-y-4 rounded-2xl bg-white p-6 text-center shadow">
			<p class="text-lg text-red-700" role="alert">{error}</p>
			<a href="/" class="inline-block rounded-xl bg-brand-600 px-6 py-3 font-semibold text-white">กลับหน้าเข้าสู่ระบบ</a>
		</div>
	{:else}
		<p class="text-stone-500">กำลังเข้าสู่ระบบ…</p>
	{/if}
</main>
