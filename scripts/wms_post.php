<?php
// Helper for scripts/e2e_wip_flow.sh: post one WMS movement the way a WMS flow does, tagged with the
// reference_type that flow writes. Usage inside tinker:
//   wms_post('cut', 'RM-PIPE', 'WH-DECHA-POST', 18)   // RM tablet "ตัด"     -> warehouse_floor_cut
//   wms_post('receive', 'FG-POST', 'WH-DECHA-POST', 10) // tablet "รับเข้า" -> warehouse_floor_receive
use App\Enums\StockLedgerMovement as M;
use App\Models\Product;
use App\Models\Warehouse;
use App\Services\Inventory\InventoryPostingService;
use App\Services\Inventory\PostingContext;

if (! function_exists('wms_post')) {
	function wms_post(string $kind, string $sku, string $wh, float $qty): void
	{
		$svc = app(InventoryPostingService::class);
		$product = Product::where('sku', $sku)->firstOrFail();
		$warehouseId = Warehouse::where('code', $wh)->value('id');
		$ref = $kind === 'cut' ? 'warehouse_floor_cut' : 'warehouse_floor_receive';
		$ctx = new PostingContext($ref, null, "e2e {$kind}", null, 'wip-e2e');
		$kind === 'cut'
			? $svc->issue($product, $warehouseId, $qty, M::Issue, $ctx)
			: $svc->receive($product, $warehouseId, $qty, M::Receive, $ctx);
		echo 'LEDGER_MAX=', \App\Models\StockLedger::max('id'), "\n";
	}
}
