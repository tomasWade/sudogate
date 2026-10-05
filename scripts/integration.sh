#!/bin/bash
set -uo pipefail

cd "$(dirname "$0")/.."
SOCK=${SUDOGATE_IT_SOCK:-/tmp/sudogate-it-$$.sock}
SERVER=./build/sudogate-server
CLIENT=./build/sudogate-client
PASS=0
FAIL=0

cleanup() {
	[ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null
	wait 2>/dev/null
	rm -f "$SOCK" "$SOCK.ctl"
}
trap cleanup EXIT

start_server() {
	rm -f "$SOCK" "$SOCK.ctl"
	$SERVER serve -socket "$SOCK" "$@" 2>/tmp/sudogate-it-server.log &
	SERVER_PID=$!
	sleep 0.4
}

get_first_id() {
	$SERVER status -socket "$SOCK" | awk 'NR==1{print $1}'
}

check() {
	local name=$1 expect=$2 got=$3
	if [ "$expect" = "$got" ]; then
		echo "PASS: $name"
		PASS=$((PASS+1))
	else
		echo "FAIL: $name (expect=$expect got=$got)"
		FAIL=$((FAIL+1))
	fi
}

echo "== 场景1: 批准 =="
start_server -timeout 60
OUT=$(mktemp)
SUDOGATE_SOCK="$SOCK" $CLIENT test >"$OUT" 2>&1 &
CPID=$!
sleep 0.6
ID=$(get_first_id)
echo "test-password-123" | $SERVER approve -socket "$SOCK" -id "$ID" >/dev/null
wait $CPID; RC=$?
check "client exit" 0 $RC
grep -q "self-test OK: 收到 17 字节" "$OUT"
check "收到密码(17B)" 0 $?

echo "== 场景2: 拒绝 =="
OUT=$(mktemp)
SUDOGATE_SOCK="$SOCK" $CLIENT test >"$OUT" 2>&1 &
CPID=$!
sleep 0.6
ID=$(get_first_id)
$SERVER deny -socket "$SOCK" -id "$ID" >/dev/null
wait $CPID; RC=$?
check "client exit" 1 $RC
grep -q "denied" "$OUT"
check "拒绝原因" 0 $?

echo "== 场景3: 同命令合并 =="
OUT1=$(mktemp); OUT2=$(mktemp)
SUDOGATE_SOCK="$SOCK" $CLIENT test >"$OUT1" 2>&1 &
C1=$!
SUDOGATE_SOCK="$SOCK" $CLIENT test >"$OUT2" 2>&1 &
C2=$!
sleep 0.8
N=$($SERVER status -socket "$SOCK" | grep -c .)
check "合并为1条待批" 1 "$N"
ID=$(get_first_id)
echo "merged-pass" | $SERVER approve -socket "$SOCK" -id "$ID" >/dev/null
wait $C1; R1=$?
wait $C2; R2=$?
check "client1 exit" 0 $R1
check "client2 exit" 0 $R2

echo "== 场景4: 超时 =="
kill $SERVER_PID 2>/dev/null; wait $SERVER_PID 2>/dev/null
start_server -timeout 2
OUT=$(mktemp)
SUDOGATE_SOCK="$SOCK" $CLIENT test >"$OUT" 2>&1 &
CPID=$!
sleep 3.5
wait $CPID; RC=$?
check "client exit" 1 $RC
grep -q "timeout" "$OUT"
check "超时原因" 0 $?

kill $SERVER_PID 2>/dev/null; wait $SERVER_PID 2>/dev/null

echo "== 审计记录 =="
AUDIT=~/.local/state/sudogate/audit.jsonl
if [ -f "$AUDIT" ]; then
	tail -5 "$AUDIT"
	check "审计条数>=4" 0 $([ "$(wc -l <"$AUDIT")" -ge 4 ] && echo 0 || echo 1)
else
	echo "未找到审计文件: $AUDIT"
	check "审计存在" 0 1
fi

echo
echo "结果: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
