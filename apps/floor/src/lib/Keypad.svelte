<script lang="ts">
	// Big-button number entry for gloved hands; value is a decimal string.
	let { value = $bindable('0'), disabled = false }: { value?: string; disabled?: boolean } = $props();

	function press(k: string) {
		if (disabled) return;
		if (k === '⌫') value = value.length > 1 ? value.slice(0, -1) : '0';
		else if (k === '.') value = value.includes('.') ? value : value + '.';
		else value = value === '0' ? k : value.length < 9 ? value + k : value;
	}
</script>

<div class="grid grid-cols-3 gap-2">
	{#each ['7', '8', '9', '4', '5', '6', '1', '2', '3', '.', '0', '⌫'] as k (k)}
		<button
			type="button"
			{disabled}
			onclick={() => press(k)}
			class="h-16 rounded-xl bg-white text-2xl font-semibold shadow-sm ring-1 ring-stone-200 active:bg-stone-200 disabled:opacity-40"
		>
			{k}
		</button>
	{/each}
</div>
