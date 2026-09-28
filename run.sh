#!/usr/bin/env bash
# Start and stop peekadoc as a detached background process.
#
#   ./run.sh start | stop | restart | status | logs
#
# The server runs in its own session, so it keeps running after the shell that
# started it exits. Settings come from environment variables, or from a .env
# file next to this script (see .env.example):
#
#   PEEKADOC_ROOT  folder to serve                      (default: $HOME)
#   PEEKADOC_ADDR  listen address                       (default: <tailscale-ip>:$PEEKADOC_PORT)
#   PEEKADOC_PORT  port used with the detected address  (default: 8000)
#   PEEKADOC_ARGS  extra flags, e.g. "-no-download"
set -euo pipefail

cd "$(dirname "$0")"
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

BIN=./peekadoc
RUN_DIR=.cache
PID_FILE=$RUN_DIR/peekadoc.pid
ADDR_FILE=$RUN_DIR/peekadoc.addr
URL_FILE=$RUN_DIR/peekadoc.url
LOG_FILE=$RUN_DIR/peekadoc.log

# The App Store build's `tailscale` CLI can crash from a terminal, so fall back
# to the app's own binary.
tailscale_ip() {
  local cli ip
  for cli in tailscale /Applications/Tailscale.app/Contents/MacOS/Tailscale; do
    ip=$("$cli" ip -4 2>/dev/null | head -n 1) || continue
    if [[ $ip == 100.* ]]; then
      echo "$ip"
      return 0
    fi
  done
  return 1
}

# Prints the server's pid if it is running.
running_pid() {
  [[ -f $PID_FILE ]] || return 1
  local pid
  pid=$(<"$PID_FILE")
  [[ -n $pid ]] && ps -p "$pid" -o command= 2>/dev/null | grep -q peekadoc && echo "$pid"
}

url() {
  cat "$URL_FILE" 2>/dev/null || echo 'https://?/'
}

tailscale_dns() {
  local cli dns
  for cli in tailscale /Applications/Tailscale.app/Contents/MacOS/Tailscale; do
    dns=$("$cli" status --json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))' 2>/dev/null) || continue
    if [[ -n $dns ]]; then echo "$dns"; return 0; fi
  done
  return 1
}

build() {
  if [[ ! -x $BIN ]] ||
    [[ -n $(find . -maxdepth 1 \( -name '*.go' -o -name app.html -o -name go.mod -o -name go.sum \) -newer "$BIN") ]]; then
    echo "Building peekadoc..."
    go build -o "$BIN" .
  fi
}

start() {
  local pid
  if pid=$(running_pid); then
    echo "peekadoc is already running (pid $pid) at $(url)"
    return 0
  fi
  build

  local root=${PEEKADOC_ROOT:-$HOME} addr=${PEEKADOC_ADDR:-} ip dns server_url
  dns=${PEEKADOC_TLS_NAME:-}
  if [[ -z $dns ]]; then dns=$(tailscale_dns) || { echo "Tailscale MagicDNS name not found" >&2; return 1; }; fi
  if [[ -z $addr ]]; then
    if ip=$(tailscale_ip); then
      addr=$ip:${PEEKADOC_PORT:-8000}
    else
      echo "Tailscale IP not found; connect Tailscale before starting peekadoc." >&2
      return 1
    fi
  fi

  mkdir -p "$RUN_DIR"
  echo "$addr" >"$ADDR_FILE"
  server_url="https://$dns:${addr##*:}/"
  echo "$server_url" >"$URL_FILE"
  echo "--- $(date '+%F %T') start: root=$root addr=$addr ${PEEKADOC_ARGS:-}" >>"$LOG_FILE"
  # setsid puts the server in a new session, detached from this shell's
  # process group and terminal; nohup ignores SIGHUP as a second guard.
  # shellcheck disable=SC2086
  nohup perl -MPOSIX=setsid -e 'setsid(); exec @ARGV or die "exec: $!\n"' \
    "$BIN" -root "$root" -addr "$addr" -tls-name "$dns" ${PEEKADOC_ARGS:-} >>"$LOG_FILE" 2>&1 </dev/null &
  echo $! >"$PID_FILE"

  for _ in $(seq 1 30); do
    if ! running_pid >/dev/null; then
      echo "peekadoc exited during startup. Last log lines:"
      tail -n 20 "$LOG_FILE"
      rm -f "$PID_FILE"
      return 1
    fi
    if curl -fsS -o /dev/null "$server_url" 2>/dev/null; then
      echo "peekadoc started (pid $(<"$PID_FILE")) at $(url)"
      echo "Logs: $LOG_FILE"
      return 0
    fi
    sleep 1
  done
  echo "peekadoc is running (pid $(<"$PID_FILE")) but not answering at $(url) yet; see $LOG_FILE"
}

stop() {
  local pid
  if ! pid=$(running_pid); then
    echo "peekadoc is not running"
    rm -f "$PID_FILE"
    return 0
  fi
  kill -TERM "$pid" # graceful: the server also stops its MkDocs worker
  for _ in $(seq 1 20); do
    if ! kill -0 "$pid" 2>/dev/null; then
      rm -f "$PID_FILE"
      echo "peekadoc stopped"
      return 0
    fi
    sleep 0.5
  done
  echo "Still running after 10s; sending SIGKILL"
  kill -KILL "$pid"
  rm -f "$PID_FILE"
}

status() {
  local pid
  if pid=$(running_pid); then
    echo "peekadoc is running (pid $pid) at $(url)"
  else
    echo "peekadoc is not running"
    return 3
  fi
}

case ${1:-} in
  start) start ;;
  stop) stop ;;
  restart) stop && start ;;
  status) status ;;
  logs) tail -n 50 -f "$LOG_FILE" ;;
  *)
    echo "Usage: $0 {start|stop|restart|status|logs}" >&2
    exit 2
    ;;
esac
