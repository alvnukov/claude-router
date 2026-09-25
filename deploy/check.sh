#!/usr/bin/env bash
# End-to-end check of `./router deploy` under real launchd and Caddy.
#
#   deploy/check.sh          disposable run: its own directory, ports and labels
#   deploy/check.sh --live   check the router installed in ROUTER_HOME
#
# The disposable run starts a legacy router under launchd, cuts it over with
# `./router install --cutover`, and keeps a long event stream open through
# Caddy while Caddy reloads and `./router deploy -force` moves it to the other
# slot with the same build. The stream must end intact, the next request must
# reach a new PID, the old process must be gone and its label unloaded, Caddy
# restarted by launchd must still route to the new slot, and a repeat deploy
# must change nothing. On exit it unloads what it loaded; on success it also
# removes its directory.
#
# Labels come from `localrouter service-labels`, which reads deploy.json or
# names a proposed prefix; the live names are refused without --live. Before
# its first launchctl call that changes anything, the disposable run checks
# that none of its labels is loaded, that its ports are free and not the live
# router's, and that its directory lies outside the router home.
#
# --live runs the same deploy checks against the installed router, without
# the stream and the Caddy restart: there, a working session answering during
# the deploy is the stream.
set -euo pipefail
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
USAGE="usage: deploy/check.sh [--live]"
LIVE=0
[ "$#" -le 1 ] || { echo "$USAGE" >&2; exit 2; }
case "${1:-}" in
  "") ;;
  --live) LIVE=1 ;;
  *) echo "$USAGE" >&2; exit 2 ;;
esac

say()  { printf 'check: %s\n' "$*"; }
fail() { printf 'check: FAIL: %s\n' "$*" >&2; exit 1; }
field() { /usr/bin/plutil -extract "$1" raw -o - - 2>/dev/null || true; }  # one JSON field from stdin
loaded() { launchctl print "gui/${UID}/$1" >/dev/null 2>&1; }
# retry N CMD...: run CMD every 0.2s until it succeeds, at most N times.
retry() { local n=$1; shift; for _ in $(seq "$n"); do "$@" && return 0; sleep 0.2; done; return 1; }
healthz() { curl -sf --max-time 5 "http://$PUBLIC_API/healthz"; }
serves() { [ "$(healthz | field slot)" = "$1" ]; }

WORK=""
KEEP=0        # 1 once WORK holds logs worth keeping after a failure
LAUNCHD=0     # 1 once the preflight passed; from then on OWN_LABELS may be loaded
STREAM_EVENTS=80
OWN_LABELS=()
UPSTREAM_PID=""
STREAM_PID=""
cleanup() {
  local status=$?
  if [ -n "$STREAM_PID" ]; then kill "$STREAM_PID" 2>/dev/null || true; fi
  if [ "$LAUNCHD" = 1 ]; then
    # The preflight saw none of these loaded, so whatever is loaded now is ours.
    for label in "${OWN_LABELS[@]}"; do
      if loaded "$label"; then launchctl bootout "gui/${UID}/$label" 2>/dev/null || true; fi
    done
  fi
  if [ -n "$UPSTREAM_PID" ]; then kill "$UPSTREAM_PID" 2>/dev/null || true; fi
  if [ -n "$WORK" ]; then
    if [ "$status" = 0 ] || [ "$KEEP" = 0 ]; then rm -rf "$WORK"; else echo "check: files kept in $WORK" >&2; fi
  fi
  exit "$status"
}
trap cleanup EXIT

# load_labels JSON sets the label names from service-labels output.
load_labels() {
  LEGACY_LABEL="$(printf '%s' "$1" | field prefix)"
  BLUE_LABEL="$(printf '%s' "$1" | field blue)"
  GREEN_LABEL="$(printf '%s' "$1" | field green)"
  CADDY_LABEL="$(printf '%s' "$1" | field caddy)"
  DEFAULT_PREFIX="$(printf '%s' "$1" | field default_prefix)"
  if [ -z "$LEGACY_LABEL" ] || [ -z "$BLUE_LABEL" ] || [ -z "$GREEN_LABEL" ] || [ -z "$CADDY_LABEL" ]; then
    fail "service-labels named no labels: $1"
  fi
}

scratch_setup() {
  local router_dir port
  router_dir="${ROUTER_HOME:-$HOME/.claude/local-router}"
  router_dir="$(cd "$router_dir" 2>/dev/null && pwd -P || printf '%s' "$router_dir")"
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/router-check.XXXXXX")"
  WORK="$(cd "$WORK" && pwd -P)"
  case "$WORK/" in "$router_dir"/*) fail "scratch directory $WORK lies inside the router home $router_dir" ;; esac

  say "building the router and the check helper in $WORK"
  mkdir -p "$WORK/bin"
  (cd "$SRC" && go build -o "$WORK/bin/localrouter" . && go build -o "$WORK/bin/checkhelper" ./deploy/checkhelper)

  PREFIX="${CHECK_LABEL_PREFIX:-router-check.$$-$RANDOM}"
  local labels
  labels="$("$WORK/bin/localrouter" service-labels -prefix "$PREFIX")" || fail "invalid label prefix $PREFIX"
  load_labels "$labels"
  [ "$DEFAULT_PREFIX" = false ] || fail "$LEGACY_LABEL names the live router's labels; use --live to check the live router"

  read -r API_PORT UI_PORT BLUE_API_PORT BLUE_UI_PORT GREEN_API_PORT GREEN_UI_PORT ADMIN_PORT <<<"$("$WORK/bin/checkhelper" ports 7)" || true
  for port in "$API_PORT" "$UI_PORT" "$BLUE_API_PORT" "$BLUE_UI_PORT" "$GREEN_API_PORT" "$GREEN_UI_PORT" "$ADMIN_PORT"; do
    case "$port" in
      '' | *[!0-9]*) fail "the check helper gave no seven ports" ;;
      8787 | 8788 | 8791 | 8792 | 8793 | 8794) fail "port $port belongs to the live router" ;;
    esac
    if nc -z -G 1 127.0.0.1 "$port" 2>/dev/null; then fail "port $port is already in use"; fi
  done
  CADDY="$(command -v caddy)" || fail "caddy is not installed"
  for label in "$LEGACY_LABEL" "$BLUE_LABEL" "$GREEN_LABEL" "$CADDY_LABEL"; do
    if loaded "$label"; then fail "launchd label $label is already loaded; leaving it alone"; fi
  done
  OWN_LABELS=("$CADDY_LABEL" "$BLUE_LABEL" "$GREEN_LABEL" "$LEGACY_LABEL")
  LAUNCHD=1
  KEEP=1

  PUBLIC_API="127.0.0.1:$API_PORT"
  PUBLIC_UI="127.0.0.1:$UI_PORT"
  ADMIN="127.0.0.1:$ADMIN_PORT"
  HOME_DIR="$WORK/home"
  AGENTS="$WORK/agents"
  mkdir -p "$HOME_DIR" "$AGENTS" "$WORK/codex" "$WORK/claude"

  "$WORK/bin/checkhelper" upstream >"$WORK/upstream.addr" 2>"$WORK/upstream.log" &
  UPSTREAM_PID=$!
  retry 50 test -s "$WORK/upstream.addr" || fail "the stream upstream did not start"
  local upstream
  upstream="$(head -1 "$WORK/upstream.addr")"
  # Every path the router could take from HOME points into the scratch
  # directory, and every upstream is the local stream stub.
  cat >"$HOME_DIR/env" <<EOF
ROUTER_LISTEN=$PUBLIC_API
ROUTER_UI_LISTEN=$PUBLIC_UI
ROUTER_BLUE_API=127.0.0.1:$BLUE_API_PORT
ROUTER_BLUE_UI=127.0.0.1:$BLUE_UI_PORT
ROUTER_GREEN_API=127.0.0.1:$GREEN_API_PORT
ROUTER_GREEN_UI=127.0.0.1:$GREEN_UI_PORT
ROUTER_CADDY_ADMIN=$ADMIN
ROUTER_UPSTREAM_URL=http://$upstream
ROUTER_LOCAL_BASE_URL=http://$upstream/v1
ROUTER_LOCAL_PROBE_INTERVAL=0
ROUTER_CODEX_AUTH_FILE=$HOME_DIR/codex-auth.json
CODEX_HOME=$WORK/codex
CLAUDE_CONFIG_DIR=$WORK/claude
EOF
  cp "$WORK/bin/localrouter" "$HOME_DIR/localrouter"
  cat >"$AGENTS/$LEGACY_LABEL.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LEGACY_LABEL</string>
  <key>ProgramArguments</key>
  <array><string>$HOME_DIR/localrouter</string></array>
  <key>WorkingDirectory</key><string>$HOME_DIR</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$HOME_DIR/router.log</string>
  <key>StandardErrorPath</key><string>$HOME_DIR/router.log</string>
</dict>
</plist>
EOF
  say "starting a legacy router as $LEGACY_LABEL on $PUBLIC_API"
  launchctl bootstrap "gui/${UID}" "$AGENTS/$LEGACY_LABEL.plist" || fail "could not load $LEGACY_LABEL"
  retry 100 curl -sf --max-time 2 -o /dev/null "http://$PUBLIC_UI/status" || fail "the legacy router did not answer; see $HOME_DIR/router.log"

  say "cutting over to Caddy and blue"
  if ! printf 'yes\n' | ROUTER_HOME="$HOME_DIR" "$SRC/router" install --cutover -agents "$AGENTS" -label-prefix "$PREFIX" -wait 1m >"$WORK/cutover.log" 2>&1; then
    tail -20 "$WORK/cutover.log" >&2
    fail "cutover"
  fi
  DEPLOY=(env "ROUTER_HOME=$HOME_DIR" "$SRC/router" deploy -agents "$AGENTS")
}

live_setup() {
  HOME_DIR="${ROUTER_HOME:-$HOME/.claude/local-router}"
  [ -f "$HOME_DIR/deploy.json" ] || fail "$HOME_DIR/deploy.json is missing; this router has not been cut over"
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/router-check.XXXXXX")"
  KEEP=1
  PUBLIC_API="$(field public_api <"$HOME_DIR/deploy.json")"
  DEPLOY=(live_deploy)
}

# live_deploy redeploys the build the active slot runs, not this checkout, so
# the check never ships code nobody installed.
live_deploy() {
  local bin
  bin="$HOME_DIR/localrouter.$(cat "$HOME_DIR/active-slot")"
  "$bin" deploy -home "$HOME_DIR" -binary "$bin" "$@"
}

start_stream() {
  curl -sSN --max-time 300 "http://$PUBLIC_API/check/stream?events=$STREAM_EVENTS&every=500ms" >"$WORK/stream.out" 2>"$WORK/stream.err" &
  STREAM_PID=$!
  retry 50 grep -qx 'data: 0' "$WORK/stream.out" || fail "the stream did not start through Caddy"
}

stream_grew() { [ "$(grep -c '^data: ' "$WORK/stream.out")" -gt "$1" ]; }
stream_done() { grep -qx 'event: done' "$WORK/stream.out"; }

reload_caddy() {
  local seen
  seen="$(grep -c '^data: ' "$WORK/stream.out")"
  say "reloading Caddy mid-stream"
  "$CADDY" reload --config "$HOME_DIR/Caddyfile" --adapter caddyfile --address "$ADMIN" --force >"$WORK/reload.log" 2>&1 || fail "caddy reload; see $WORK/reload.log"
  retry 25 stream_grew "$seen" || fail "the stream stalled after the Caddy reload"
}

check_stream() {
  local status=0
  wait "$STREAM_PID" || status=$?
  STREAM_PID=""
  [ "$status" = 0 ] || fail "the stream broke (curl exit $status: $(cat "$WORK/stream.err"))"
  { for i in $(seq 0 $((STREAM_EVENTS - 1))); do printf 'data: %d\n\n' "$i"; done; printf 'event: done\ndata: %d\n\n' "$STREAM_EVENTS"; } >"$WORK/stream.want"
  cmp -s "$WORK/stream.want" "$WORK/stream.out" || fail "the stream lost or repeated events; compare $WORK/stream.want and $WORK/stream.out"
  say "the stream finished with every event in order"
}

check_deploy() {
  local labels before old_pid old_slot old_label next_slot deploy_pid status after new_pid new_slot
  labels="$("$HOME_DIR/localrouter.$(cat "$HOME_DIR/active-slot")" service-labels -home "$HOME_DIR")" || fail "service-labels from deploy.json"
  load_labels "$labels"
  if [ "$LIVE" = 0 ] && [ "$CADDY_LABEL $BLUE_LABEL $GREEN_LABEL $LEGACY_LABEL" != "${OWN_LABELS[*]}" ]; then
    fail "deploy.json names other labels than the ones this run checked: $labels"
  fi
  before="$(healthz)" || fail "nothing answers on $PUBLIC_API/healthz"
  old_pid="$(printf '%s' "$before" | field pid)"
  old_slot="$(printf '%s' "$before" | field slot)"
  case "$old_slot" in
    blue) old_label="$BLUE_LABEL" next_slot=green ;;
    green) old_label="$GREEN_LABEL" next_slot=blue ;;
    *) fail "unexpected /healthz: $before" ;;
  esac
  say "slot $old_slot serves as PID $old_pid"

  if [ "$LIVE" = 0 ]; then
    start_stream
    reload_caddy
  fi
  say "deploying the same build with -force"
  "${DEPLOY[@]}" -force >"$WORK/deploy.log" 2>&1 &
  deploy_pid=$!
  until serves "$next_slot"; do
    kill -0 "$deploy_pid" 2>/dev/null || break
    sleep 0.2
  done
  if [ "$LIVE" = 0 ] && serves "$next_slot"; then
    stream_done && fail "the stream ended before the switch; it proves nothing"
    say "Caddy switched while the stream was open"
  fi
  status=0
  wait "$deploy_pid" || status=$?
  [ "$status" = 0 ] || { tail -20 "$WORK/deploy.log" >&2; fail "forced deploy (exit $status)"; }
  if [ "$LIVE" = 0 ]; then check_stream; fi

  after="$(healthz)" || fail "nothing answers after the deploy"
  new_pid="$(printf '%s' "$after" | field pid)"
  new_slot="$(printf '%s' "$after" | field slot)"
  [ -n "$new_pid" ] && [ "$new_pid" != "$old_pid" ] && [ "$new_slot" = "$next_slot" ] || fail "no switch: before $before, after $after"
  say "slot $new_slot now serves as PID $new_pid"
  if kill -0 "$old_pid" 2>/dev/null; then fail "old PID $old_pid is still running"; fi
  if loaded "$old_label"; then fail "old label $old_label is still loaded"; fi
  say "old PID $old_pid exited and $old_label is unloaded"

  if [ "$LIVE" = 0 ]; then
    launchctl kickstart -k "gui/${UID}/$CADDY_LABEL" || fail "restart Caddy"
    retry 100 serves "$new_slot" || fail "Caddy restarted by launchd does not route to $new_slot"
    say "Caddy restarted by launchd still routes to $new_slot"
  fi

  "${DEPLOY[@]}" >"$WORK/repeat.log" 2>&1 || { cat "$WORK/repeat.log" >&2; fail "repeat deploy"; }
  grep -q '^no change' "$WORK/repeat.log" || fail "repeat deploy was not a no-op: $(cat "$WORK/repeat.log")"
  [ "$(healthz | field pid)" = "$new_pid" ] || fail "repeat deploy replaced PID $new_pid"
  say "repeat deploy: $(tail -1 "$WORK/repeat.log")"
}

if [ "$LIVE" = 1 ]; then live_setup; else scratch_setup; fi
check_deploy
say "passed"
