#!/bin/bash
# 短枝损耗复核 · 端到端验证脚本（干净库）
set -e
BASE=${BASE:-http://localhost:8080}
J() { python3 -c "import sys,json;d=json.load(sys.stdin);$1"; }
tok() { curl -s $BASE/api/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$1\",\"password\":\"flower123\"}" | J 'print(d["token"])'; }
T_FARMER=$(tok farmer01); T_QC=$(tok qc01); T_FIN=$(tok fin01); T_SHOP=$(tok shop01)
T_BOSS=$(tok boss01); T_COLD=$(tok cold01); T_W2=$(tok wholesaler02); T_SHIP=$(tok ship01)
PASS=0; FAIL=0
chk() { if [ "$2" = "$3" ]; then echo "PASS: $1 = $2"; PASS=$((PASS+1)); else echo "FAIL: $1 got=[$2] want=[$3]"; FAIL=$((FAIL+1)); fi; }
nchk() { python3 -c "
import sys
got,want=float('$2'),float('$3')
ok=abs(got-want)<1e-6
print(('PASS: ' if ok else 'FAIL: ')+'$1 = '+str(got))
sys.exit(0 if ok else 1)" && PASS=$((PASS+1)) || FAIL=$((FAIL+1)); }

echo "===== 0. 初始 seed：花农老张账扣状态同步 ====="
curl -s $BASE/api/short-stem-reviews -H "Authorization: Bearer $T_FARMER" > /tmp/s0.json
chk "seed 复核单数" "$(J 'print(len(d["items"]))' < /tmp/s0.json)" "4"
nchk "已账扣金额" "$(J 'print(d["summary"]["charged_amount"])' < /tmp/s0.json)" "105.6"
nchk "争议冻结金额" "$(J 'print(d["summary"]["frozen_amount"])' < /tmp/s0.json)" "96.0"

echo "===== 1. 新玫瑰批次 → 竞价结拍（自动核销 seed 中 10% 下一批折扣） ====="
BID=$(curl -s $BASE/api/batches -X POST -H "Authorization: Bearer $T_FARMER" -H 'Content-Type: application/json' \
 -d '{"category":"rose","origin":"云南玉溪","cut_at":"2026-09-25T02:30","stem_length_cm":66,"openness":3,"bundles":30,"stems_per_bundle":20,"arrival_temp_c":4.5,"sellable_hours":48}' | J 'print(d["id"])')
curl -s $BASE/api/batches/$BID/qc -X POST -H "Authorization: Bearer $T_QC" -H 'Content-Type: application/json' \
 -d '{"grade":"A","openness_evenness":0.92,"short_stem_ratio":0.06,"temp_ok":true,"notes":"复检"}' >/dev/null
curl -s $BASE/api/batches/$BID/store -X POST -H "Authorization: Bearer $T_COLD" -H 'Content-Type: application/json' -d '{"slot_id":4}' >/dev/null
AUC=$(curl -s $BASE/api/auctions -X POST -H "Authorization: Bearer $T_BOSS" -H 'Content-Type: application/json' \
 -d "{\"batch_id\":$BID,\"mode\":\"auction\",\"start_price\":35,\"duration_min\":60}" | J 'print(d["id"])')
curl -s $BASE/api/auctions/$AUC/bid -X POST -H "Authorization: Bearer $T_W2" -H 'Content-Type: application/json' -d '{"price":40}' >/dev/null
curl -s $BASE/api/auctions/$AUC/close -X POST -H "Authorization: Bearer $T_BOSS" >/dev/null
nchk "承诺已核销金额 40*30*10%" "$(curl -s $BASE/api/short-stem-reviews/3 -H "Authorization: Bearer $T_FARMER" | J 'print(d["commitment"]["redeemed_amount"])')" "120.0"

echo "===== 2. 上架 → 花店下单 → 支付 → 发车（必传分拣照片）→ 送达 ====="
INV=$(curl -s $BASE/api/inventories -H "Authorization: Bearer $T_W2" | J "print([i['id'] for i in d if i['batch']['id']==$BID][0])")
curl -s $BASE/api/inventories/$INV/listing -X POST -H "Authorization: Bearer $T_W2" -H 'Content-Type: application/json' -d '{"listed":true,"list_price":52}' >/dev/null
OID=$(curl -s $BASE/api/orders -X POST -H "Authorization: Bearer $T_SHOP" -H 'Content-Type: application/json' \
 -d "{\"inventory_id\":$INV,\"bundles\":8,\"purpose\":\"retail\",\"payment_type\":\"immediate\"}" | J 'print(d["id"])')
curl -s $BASE/api/orders/$OID/pay -X POST -H "Authorization: Bearer $T_SHOP" >/dev/null
NOPHOTO=$(curl -s -o /dev/null -w '%{http_code}' $BASE/api/deliveries -X POST -H "Authorization: Bearer $T_SHIP" -H 'Content-Type: application/json' -d "{\"order_id\":$OID,\"to_city\":\"本市\"}")
chk "无分拣照片拒绝发车 400" "$NOPHOTO" "400"
DID=$(curl -s $BASE/api/deliveries -X POST -H "Authorization: Bearer $T_SHIP" -H 'Content-Type: application/json' \
 -d "{\"order_id\":$OID,\"to_city\":\"本市\",\"sorting_photos\":\"https://x/s-a.jpg,https://x/s-b.jpg\"}" | J 'print(d["id"])')
curl -s $BASE/api/deliveries/$DID/status -X POST -H "Authorization: Bearer $T_SHIP" -H 'Content-Type: application/json' -d '{"status":"delivered"}' >/dev/null
chk "配送单留存分拣照片数" "$(curl -s $BASE/api/deliveries -H "Authorization: Bearer $T_SHIP" | J "print(len([d for d in d if d['id']==$DID][0]['sorting_photos']))")" "2"

echo "===== 3. 花店发起短枝复核（服务汇聚四组证据） ====="
LOW=$(curl -s -o /dev/null -w '%{http_code}' $BASE/api/orders/$OID/short-stem-reviews -X POST -H "Authorization: Bearer $T_SHOP" -H 'Content-Type: application/json' \
 -d '{"bundles":8,"actual_short_ratio":0.05,"reshoot_photos":["https://x/r.jpg"]}')
chk "实测未高于说明拒绝 409" "$LOW" "409"
SR=$(curl -s $BASE/api/orders/$OID/short-stem-reviews -X POST -H "Authorization: Bearer $T_SHOP" -H 'Content-Type: application/json' \
 -d '{"bundles":8,"actual_short_ratio":0.26,"reshoot_photos":["https://x/r1.jpg","https://x/r2.jpg"],"remark":"短枝偏多"}')
RID=$(echo "$SR" | J 'print(d["id"])')
chk "竞价说明短枝率快照" "$(echo "$SR" | J 'print(d["declared_short_ratio"])')" "0.06"
chk "到店实测短枝率" "$(echo "$SR" | J 'print(d["actual_short_ratio"])')" "0.26"
DETAIL=$(curl -s $BASE/api/short-stem-reviews/$RID -H "Authorization: Bearer $T_QC")
chk "证据①分拣照片" "$(echo "$DETAIL" | J 'print(len(d["evidence_arrival"]["sorting_photos"]))')" "2"
chk "证据②质检等级" "$(echo "$DETAIL" | J 'print(d["evidence_arrival"]["qc_grade"])')" "A"
chk "证据③出库扎数" "$(echo "$DETAIL" | J 'print(d["evidence_arrival"]["outbound_bundles"])')" "8"
chk "证据④复拍照片" "$(echo "$DETAIL" | J 'print(len(d["evidence_store"]["reshoot_photos"]))')" "2"

echo "===== 4. 质检确认差价补偿（52*(0.26-0.06)=10.4/扎×8=83.2） ====="
CF=$(curl -s $BASE/api/short-stem-reviews/$RID/confirm -X POST -H "Authorization: Bearer $T_QC" -H 'Content-Type: application/json' -d '{"resolution":"price_diff","note":"两组证据比对属实"}')
chk "差价金额自动核算" "$(echo "$CF" | J 'print(d["price_diff_amount"])')" "83.2"
chk "确认后待账扣" "$(echo "$CF" | J 'print(d["status"])')" "confirmed"

echo "===== 5. 花农异议 → 财务冻结 83.2 ====="
DP=$(curl -s $BASE/api/short-stem-reviews/$RID/dispute -X POST -H "Authorization: Bearer $T_FARMER" -H 'Content-Type: application/json' -d '{"reason":"分拣照片枝长达标，申请复尺"}')
chk "争议冻结金额" "$(echo "$DP" | J 'print(d["frozen_amount"])')" "83.2"
chk "争议状态" "$(echo "$DP" | J 'print(d["status"])')" "disputed"

echo "===== 6. 财务维持原判 → 花农确认账扣 ====="
curl -s $BASE/api/short-stem-reviews/$RID/close-dispute -X POST -H "Authorization: Bearer $T_FIN" -H 'Content-Type: application/json' -d '{"uphold":true,"note":"复尺超标，维持"}' >/dev/null
AK=$(curl -s $BASE/api/short-stem-reviews/$RID/ack-charge -X POST -H "Authorization: Bearer $T_FARMER")
chk "账扣最终状态" "$(echo "$AK" | J 'print(d["status"])')" "charged"

echo "===== 7. 月度结算：账扣/让利/返还 均入明细（争议中不入账） ====="
curl -s $BASE/api/settlements/generate -X POST -H "Authorization: Bearer $T_FIN" -H 'Content-Type: application/json' -d '{"month":"2026-09"}' >/dev/null
SID=$(curl -s "$BASE/api/settlements?month=2026-09" -H "Authorization: Bearer $T_FIN" | J "print([s['id'] for s in d if s['entity_role']=='farmer' and s['entity_name'].find('老张')>=0][0])")
# 花农短枝明细 = SSR1差价105.6（seed已账扣）+ SSR3下批折扣让利120（步骤1核销）+ 新单差价83.2；SSR2 的 96 因争议冻结不入账
nchk "花农短枝账扣+让利合计" "$(curl -s $BASE/api/settlements/$SID -H "Authorization: Bearer $T_FIN" | J 'print(round(sum(abs(l["amount"]) for l in d["lines"] if l["kind"].startswith("short_stem") or l["kind"]=="next_batch_discount"),2))')" "308.8"
nchk "争议冻结96未计入结算" "$(curl -s $BASE/api/short-stem-reviews/2 -H "Authorization: Bearer $T_FARMER" | J 'print(d["frozen_amount"])')" "96.0"

echo
echo "RESULT: PASS=$PASS FAIL=$FAIL"
[ $FAIL -eq 0 ]
