#!/usr/bin/env bash
# Real-run scenario: builds karga-server and the karga CLI and drives them against the local Docker
# daemon, checking the business logic end to end. Linux only (the gateway and release proxy need it);
# from macOS run it in the VM: make test-linux TARGETS=scenario.
#
#   scripts/scenario.sh      KARGA_CLI_SRC=dir  the karga CLI checkout (default ../karga-cli)
#
# Steps: redis install and replace upgrade; postgres install, volume backup, restore and
# undo-restore; repairs of a removed and a stopped container; rollback; kill -9 during an upgrade
# and the resume at start; uninstall; the host gateway serving an exposed service; gateway
# document apply and history; a missing image (exit 4); list and table output. Leaves nothing
# behind. Exit 0 only if every check passed.
set -uo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cli=${KARGA_CLI_SRC:-$root/../karga-cli}
W=$(mktemp -d)
H=$W/home
export KARGA_SOCKET=$W/karga.sock
EX=$root/tests/testdata
fail=0
pass() { echo "PASS $*"; }
bad() { echo "FAIL $*"; fail=1; }
k() { "$W/karga" "$@"; }

# expect CODE DESC CMD...: CMD exits with CODE.
expect() {
  local want=$1 desc=$2; shift 2
  local out code
  out=$("$@" 2>&1); code=$?
  if [ "$code" -eq "$want" ]; then pass "$desc"; else bad "$desc (exit $code, want $want): $(echo "$out" | tail -5)"; fi
  LAST=$out
}
# has DESC NEEDLE: the last expect's output contains NEEDLE.
has() { if grep -qF -- "$2" <<<"$LAST"; then pass "$1"; else bad "$1: no '$2' in: $(echo "$LAST" | tail -5)"; fi; }
status_of() { k status "$1" -o json 2>/dev/null | grep -o '"status": *"[a-z]*"' | head -1 | sed 's/.*"\([a-z]*\)"$/\1/'; }
is_status() { local s; s=$(status_of "$1"); if [ "$s" = "$2" ]; then pass "$1 is $2"; else bad "$1 is '$s', want $2"; fi; }
container() { docker ps -a --filter "label=io.karga.release=$1" --filter "label=io.karga.kind=Workload" --filter "label=io.karga.name=$2" --format '{{.Names}}' | head -1; }
psql() { k exec scn-pg postgres -- psql -U app -d app -tAc "$1" 2>&1 | tr -d '[:space:]'; }

start_server() {
  "$W/karga-server" --home "$H" --socket "$KARGA_SOCKET" >>"$W/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 100); do k list >/dev/null 2>&1 && return 0; sleep 0.2; done
  bad "karga-server did not start: $(tail -5 "$W/server.log")"; exit 1
}
cleanup() {
  for r in scn-r scn-pg scn-gw scn-bad scn-out; do k uninstall "$r" --delete-volumes --yes >/dev/null 2>&1; done
  kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null
  for r in scn-r scn-pg scn-gw scn-bad scn-out; do
    ids=$(docker ps -aq --filter "label=io.karga.release=$r"); [ -n "$ids" ] && docker rm -f $ids >/dev/null
    vols=$(docker volume ls -q --filter "label=io.karga.release=$r"); [ -n "$vols" ] && docker volume rm $vols >/dev/null
  done
}

(cd "$root" && go build -o "$W/karga-server" ./cmd/karga-server) || { echo "FAIL build server"; exit 1; }
(cd "$cli" && go build -o "$W/karga" ./cmd/karga) || { echo "FAIL build cli"; exit 1; }
start_server
trap cleanup EXIT

echo "== 1 install (redis, replace strategy by upgrade flag)"
expect 0 "install scn-r" k install scn-r "$EX/redis"
is_status scn-r deployed
old=$(container scn-r redis)

echo "== 2 replace upgrade"
expect 0 "upgrade scn-r --strategy replace" k upgrade scn-r "$EX/redis" --set config.maxmemory=128mb --strategy replace
is_status scn-r deployed
new=$(container scn-r redis)
[ -n "$new" ] && [ "$new" != "$old" ] && pass "replace made a new container ($old -> $new)" || bad "replace container name: old=$old new=$new"
expect 0 "history scn-r" k history scn-r
[ "$(grep -cE '^ *[0-9]+ ' <<<"$LAST")" -ge 2 ] && pass "scn-r has 2 revisions" || bad "scn-r revisions: $LAST"

echo "== 3 postgres + volume backup"
expect 0 "install scn-pg" k install scn-pg "$EX/postgres" --set database.password=scn-pw
is_status scn-pg deployed
for _ in $(seq 30); do [ "$(psql 'select 1')" = "1" ] && break; sleep 1; done
psql "create table t(v text); insert into t values ('before')" >/dev/null
expect 0 "volume backup" k volume backup scn-pg postgres-data "$W/b.tgz"
is_status scn-pg deployed
for _ in $(seq 30); do [ "$(psql 'select v from t')" = "before" ] && break; sleep 1; done
[ "$(psql 'select v from t')" = "before" ] && pass "postgres runs again after backup" || bad "postgres after backup: $(psql 'select v from t')"

echo "== 4 restore and undo-restore"
psql "update t set v='after'" >/dev/null
expect 0 "volume restore" k volume restore scn-pg postgres-data "$W/b.tgz" --yes
for _ in $(seq 30); do [ "$(psql 'select v from t')" = "before" ] && break; sleep 1; done
[ "$(psql 'select v from t')" = "before" ] && pass "restore put back the backup" || bad "after restore: $(psql 'select v from t')"
expect 0 "volume undo-restore" k volume undo-restore scn-pg postgres-data --yes
for _ in $(seq 30); do [ "$(psql 'select v from t')" = "after" ] && break; sleep 1; done
[ "$(psql 'select v from t')" = "after" ] && pass "undo-restore put back the restore point" || bad "after undo-restore: $(psql 'select v from t')"
expect 0 "events scn-pg" k events scn-pg
has "events record volume-backup" "volume-backup"
has "events record volume-restore" "volume-restore"
[ "$(grep -c 'failed\|aborted' <<<"$LAST")" -eq 0 ] && pass "no failed scn-pg operation" || bad "scn-pg events: $LAST"

echo "== 5 repairs"
docker rm -f "$(container scn-pg postgres)" >/dev/null
expect 0 "reconcile recreates a removed container" k reconcile scn-pg
[ -n "$(container scn-pg postgres)" ] && pass "postgres container is back" || bad "postgres container missing after reconcile"
docker stop "$(container scn-pg postgres)" >/dev/null
expect 0 "reconcile starts a stopped container" k reconcile scn-pg
[ "$(docker inspect -f '{{.State.Running}}' "$(container scn-pg postgres)")" = "true" ] && pass "postgres runs after reconcile" || bad "postgres not running after reconcile"
is_status scn-pg deployed

echo "== 6 rollback"
expect 0 "rollback scn-r" k rollback scn-r
is_status scn-r deployed

echo "== 7 kill -9 during an upgrade, then resume"
k upgrade scn-r "$EX/redis" --set config.maxmemory=64mb --strategy replace >/dev/null 2>&1 &
sleep 0.7
kill -9 "$SRV"; wait "$SRV" 2>/dev/null
start_server
for _ in $(seq 90); do [ "$(status_of scn-r)" = "deployed" ] && break; sleep 1; done
is_status scn-r deployed
expect 0 "events scn-r" k events scn-r
[ "$(grep -c ' running ' <<<"$LAST")" -eq 0 ] && pass "no operation left running" || bad "scn-r events: $LAST"
if grep -q 'resuming interrupted operation.*release=scn-r' "$W/server.log"; then
  grep -q 'operation finished.*release=scn-r type=resume.*status=succeeded' "$W/server.log" && pass "the interrupted upgrade was resumed" || bad "resume did not succeed: $(grep resum "$W/server.log" | tail -3)"
else
  echo "NOTE the upgrade ended before the kill: nothing to resume"
fi

echo "== 8 uninstall"
expect 0 "uninstall scn-r" k uninstall scn-r --yes
expect 0 "uninstall scn-pg --delete-volumes" k uninstall scn-pg --delete-volumes --yes
left=$(docker ps -aq --filter label=io.karga.release=scn-r --filter label=io.karga.release=scn-pg | wc -l)
[ "$left" -eq 0 ] && pass "no containers left" || bad "$left containers left"
[ -z "$(docker volume ls -q --filter label=io.karga.release=scn-pg)" ] && pass "postgres volume deleted" || bad "postgres volume left"
[ ! -e "$H/restore-points/scn-pg" ] && pass "restore points deleted" || bad "restore points left"

echo "== 9 host gateway (host network) serves an exposed service"
expect 0 "listener add" k gateway listener add scn --port 18090 --protocol tcp --yes
expect 0 "install an exposed redis" k install scn-gw "$EX/redis" --set service.listener=scn
for _ in $(seq 30); do r=$( (exec 3<>/dev/tcp/127.0.0.1/18090 && printf 'PING\r\n' >&3 && timeout 2 head -c 7 <&3) 2>/dev/null); [ "$r" = "+PONG"$'\r' ] && break; sleep 1; done
[ "$r" = "+PONG"$'\r' ] && pass "redis answers through the host gateway" || bad "no PONG through the gateway: '$r'"
gwc=$(docker ps --filter label=io.karga.component=gateway --format '{{.Names}}' | head -1)
[ "$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$gwc" 2>/dev/null)" = "host" ] && pass "the gateway runs on the host network" || bad "gateway network: $gwc"
expect 0 "uninstall the exposed redis" k uninstall scn-gw --yes
expect 0 "listener remove" k gateway listener remove scn --yes

echo "== 10 gateway document"
expect 0 "list -o json with no releases" k list -o json
[ "$(tr -d '[:space:]' <<<"$LAST")" = "[]" ] && pass "an empty list is []" || bad "empty list: $LAST"
expect 0 "gateway get" k gateway get
printf '%s\n' "$LAST" > "$W/gw.yaml"
if grep -q 'accessLog: true' "$W/gw.yaml"; then
  sed -i 's/accessLog: true/accessLog: false/' "$W/gw.yaml"
else
  sed -i 's/accessLog: false/accessLog: true/' "$W/gw.yaml"
fi
expect 0 "gateway apply a changed document" k gateway apply -f "$W/gw.yaml" --yes -o json
has "apply returns the revision" '"revision"'
has "apply returns the actor" '"actor"'
expect 0 "gateway history -o json" k gateway history -o json
has "history lists revisions" '"revision"'

echo "== 11 a missing image is not found"
# karga-dev has no registry access, so the missing image is a local one with pullPolicy never
# (the same ports.ErrImageNotFound a registry's "manifest unknown" gives).
cp -r "$EX/redis" "$W/redis-never"
sed -i 's/^  restartPolicy:/  pullPolicy: never\n  restartPolicy:/' "$W/redis-never/templates/workload.yaml"
expect 4 "install with a missing image exits 4" k install scn-bad "$W/redis-never" --set image.repository=karga-no-such-image
has "the error names the image" "karga-no-such-image"
k uninstall scn-bad --yes >/dev/null 2>&1

echo "== 12 list and table output"
expect 0 "install for output checks" k install scn-out "$EX/redis"
expect 0 "list" k list
has "list has its header" "NAME"
has "list has the release" "scn-out"
expect 0 "list -o yaml" k list -o yaml
has "list yaml names the release" "scn-out"
expect 0 "history" k history scn-out
has "history has its header" "REVISION"
expect 0 "history -o json" k history scn-out -o json
has "history json" '"revision"'
# A fresh server has no gateway history: apply a changed document so one revision is in force.
expect 0 "gateway get" k gateway get
printf '%s\n' "$LAST" > "$W/gw10.yaml"
if grep -q 'accessLog: true' "$W/gw10.yaml"; then sed -i 's/accessLog: true/accessLog: false/' "$W/gw10.yaml"; else sed -i 's/accessLog: false/accessLog: true/' "$W/gw10.yaml"; fi
expect 0 "gateway apply" k gateway apply -f "$W/gw10.yaml" --yes
expect 0 "gateway history" k gateway history
has "gateway history header" "REVISION"
has "gateway history marks the one in force" "(in force)"
expect 0 "gateway listener list" k gateway listener list
has "listener list header" "PROTOCOL"
expect 0 "gateway listener list -o json" k gateway listener list -o json
has "listener json" '"listeners"'
expect 0 "uninstall scn-out" k uninstall scn-out --yes


echo "== server log errors"
grep -i '"level":"ERROR"\|level=ERROR' "$W/server.log" | tail -5
[ $fail -eq 0 ] && echo "SCENARIO PASS" || echo "SCENARIO FAIL"
exit $fail
