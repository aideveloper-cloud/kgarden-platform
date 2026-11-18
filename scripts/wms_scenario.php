<?php
// WMS postings for scripts/e2e_wms_bridge.sh, made through the WMS's own InventoryPostingService:
// receive, consume, transfer between warehouses (dispatch + arrive), stock count, FG receipt.
use App\Enums\StockLedgerMovement as M;
use App\Models\Product;
use App\Models\Warehouse;
use App\Services\Inventory\InventoryPostingService;
use App\Services\Inventory\PostingContext;

$svc = app(InventoryPostingService::class);
$p = fn ($sku) => Product::where('sku', $sku)->firstOrFail();
$w = fn ($code) => Warehouse::where('code', $code)->value('id');
$ctx = fn ($note) => new PostingContext('bridge_e2e', null, $note, null, 'bridge-e2e');

$pipe = $svc->onHand($p('RM-PIPE'), $w('WH-DECHA-POST'));
$svc->receive($p('RM-PIPE'), $w('WH-DECHA-POST'), 100, M::Receive, $ctx('รับท่อ 100'));
$svc->receive($p('RM-W40-W'), $w('WH-DECHA-MESH'), 500, M::Receive, $ctx('รับลวด 500 kg'));
$svc->issue($p('RM-PIPE'), $w('WH-DECHA-POST'), 30, M::Consumption, $ctx('ตัดท่อไปผลิตเสา'));
$moving = $svc->dispatch($p('RM-W40-W'), $w('WH-DECHA-MESH'), 120, $ctx('โอนลวดไปพระนอน'));
$svc->arrive($p('RM-W40-W'), $w('WH-PHRANON'), $moving, $ctx('ลวดถึงพระนอน'));
$svc->adjustTo($p('RM-PIPE'), $w('WH-DECHA-POST'), $pipe + 68, $ctx('ตรวจนับท่อ'));
$svc->receive($p('FG-POST'), $w('WH-DECHA-POST'), 10, M::Receive, $ctx('รับเสาสำเร็จรูป'));

echo 'LEDGER_MAX=', \App\Models\StockLedger::max('id'), "\n";
