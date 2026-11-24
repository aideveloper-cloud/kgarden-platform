<?php
// Helper for scripts/e2e_pos_production.sh: report output at the WMS production POS exactly as the
// tablet does (ProductionFloorService, which checks the Work Order in ERPNext). Usage inside tinker:
//   wms_report('MFG-WO-2026-00001', 'WH-DECHA-POST', 6, 0, '', 'client-key')
use App\Models\User;
use App\Models\Warehouse;
use App\Services\Production\ProductionFloorService;
use Illuminate\Validation\ValidationException;

if (! function_exists('wms_report')) {
	function wms_report(string $wo, string $wh, float $good, float $scrap, string $reason, string $key): void
	{
		$warehouse = Warehouse::where('code', $wh)->firstOrFail();
		$user = User::role('super-admin')->firstOrFail();
		try {
			$out = app(ProductionFloorService::class)->report($wo, $warehouse, $user, 'สมชาย (e2e)', $good, $scrap, $reason ?: null, $key);
			echo 'OUTPUT=', $out->id, "\nLEDGER=", $out->stock_ledger_id ?? '', "\n";
		} catch (ValidationException $e) {
			echo 'REFUSED=', collect($e->errors())->flatten()->first(), "\n";
		}
	}
}
