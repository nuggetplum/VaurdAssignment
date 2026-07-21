#!/usr/bin/env bash
#
# run.sh — one-command setup + demo for the Vaurd order processing service.
#
# Written for Git Bash on Windows (MSYS2). Run it from an Administrator
# Git Bash prompt in the repo root:
#
#     ./run.sh
#
# What it does:
#   1. Checks it's running as Administrator (needed to start Docker Desktop).
#   2. Verifies docker / go / python are present, offers to install any that
#      are missing via winget.
#   3. Starts Docker Desktop if the engine isn't already up.
#   4. Brings up Postgres + NATS and waits for both healthchecks.
#   5. Builds and starts the Go service, waits for GET /healthz.
#   6. Starts the Python generator and the Streamlit dashboard.
#   7. Prints the URLs and curl commands, then tails the service log.
#
# Ctrl+C stops everything it started and leaves the machine clean.
#
# Flags:
#   --no-dashboard   skip Streamlit
#   --no-generator   skip the event generator (service + infra only)
#   --skip-checks    skip prerequisite verification
#   --down           tear down containers and volumes, then exit

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$REPO_ROOT"

LOG_DIR="$REPO_ROOT/.run-logs"
SERVICE_LOG="$LOG_DIR/service.log"
GENERATOR_LOG="$LOG_DIR/generator.log"
DASHBOARD_LOG="$LOG_DIR/dashboard.log"

# The compiled binary deliberately does NOT live under the repo (which is
# inside OneDrive\Desktop here). OneDrive intercepts newly-written files in
# synced folders to upload them, and antivirus real-time scanning tends to
# single out freshly-built unsigned .exe files for a moment too — both can
# make a file that "go build" just reported as written vanish by the time we
# try to exec it a second later. Building to the OS temp dir sidesteps both.
BUILD_DIR="$(mktemp -d 2>/dev/null || echo "$LOG_DIR/build")"
mkdir -p "$BUILD_DIR"
SERVICE_BIN="$BUILD_DIR/vaurd-service.exe"

RUN_DASHBOARD=1
RUN_GENERATOR=1
SKIP_CHECKS=0

SERVICE_PID=""
GENERATOR_PID=""
DASHBOARD_PID=""

# ---------------------------------------------------------------- output ---

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[0;33m'
BLUE=$'\033[0;34m'; BOLD=$'\033[1m'; NC=$'\033[0m'

info()  { printf '%s==>%s %s\n' "$BLUE"   "$NC" "$*"; }
ok()    { printf '%s  ok%s %s\n' "$GREEN" "$NC" "$*"; }
warn()  { printf '%swarn%s %s\n' "$YELLOW" "$NC" "$*" >&2; }
err()   { printf '%s fail%s %s\n' "$RED"  "$NC" "$*" >&2; }

die() { err "$*"; exit 1; }

# A GUI dialog isn't something bash can wait on directly — it can appear a
# beat after the listener actually starts, and it steals focus. So instead
# of racing ahead into the next step (which can fail while the dialog is
# up), stop and make the user deal with it before continuing.
pause_for_firewall_prompt() {
  local what="$1" port="$2"
  printf '\n%sHeads up:%s Windows may pop a "Windows Defender Firewall has\n' "$YELLOW" "$NC"
  printf 'blocked some features of this app" dialog right about now, for %s\n' "$what"
  printf 'listening on port %s. It can take a few seconds to appear.\n\n' "$port"
  printf '  -> Click "Allow access" when you see it (Private networks is enough).\n'
  printf '  -> If nothing appears within a few seconds, a rule already exists\n'
  printf '     from a previous run — nothing to do.\n\n'
  read -r -t 20 -p "Press Enter once handled (or wait 20s to continue automatically): " _ </dev/tty 2>/dev/null || true
  echo
}

# ---------------------------------------------------------------- cleanup ---

cleanup() {
  local code=$?
  trap - EXIT INT TERM
  echo
  info "Shutting down..."

  for pid_var in DASHBOARD_PID GENERATOR_PID SERVICE_PID; do
    local pid="${!pid_var:-}"
    [ -z "$pid" ] && continue
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      # Give it a moment to exit gracefully, then force.
      for _ in 1 2 3 4 5 6 7 8 9 10; do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.3
      done
      kill -9 "$pid" 2>/dev/null || true
    fi
  done
  ok "stopped background processes"

  info "Stopping containers (data volumes are kept)"
  docker compose down --remove-orphans >/dev/null 2>&1 || true
  ok "containers stopped"

  if [ -n "${BUILD_DIR:-}" ] && [ -d "$BUILD_DIR" ]; then
    rm -rf "$BUILD_DIR" 2>/dev/null || true
  fi

  echo
  echo "Logs from this run are in ${LOG_DIR#$REPO_ROOT/}/"
  echo "To wipe the database too:  docker compose down -v"
  exit "$code"
}

# ------------------------------------------------------------ admin check ---

require_admin() {
  # `net session` only succeeds in an elevated shell on Windows.
  if net session >/dev/null 2>&1; then
    ok "running as Administrator"
    return
  fi

  err "This script needs Administrator rights."
  cat <<'EOF'

  Why: starting the Docker Desktop engine and binding local ports needs an
  elevated shell. Everything else would work unelevated, but Docker will
  usually fail with a "cannot find the file specified" pipe error.

  How to fix:
    1. Close this window.
    2. Windows search -> "Git Bash" -> right-click -> Run as administrator.
    3. cd into this folder and run ./run.sh again.

EOF
  exit 1
}

# ---------------------------------------------------- prerequisite checks ---

winget_install() {
  local pkg="$1" label="$2"
  info "Installing $label via winget..."
  if winget install --id "$pkg" -e --source winget \
       --accept-package-agreements --accept-source-agreements; then
    ok "$label installed"
    warn "You may need to close and reopen Git Bash so PATH picks it up."
    return 0
  fi
  err "winget could not install $label"
  return 1
}

# prompt_install <command> <winget-id> <label> <manual-url>
prompt_install() {
  local cmd="$1" pkg="$2" label="$3" url="$4"

  if command -v "$cmd" >/dev/null 2>&1; then
    ok "$label found ($(command -v "$cmd"))"
    return 0
  fi

  warn "$label is not installed or not on PATH."

  if ! command -v winget >/dev/null 2>&1; then
    err "winget is unavailable, so this can't be installed automatically."
    echo "     Install $label manually: $url"
    return 1
  fi

  local reply
  read -r -p "     Install $label now with winget? [y/N] " reply </dev/tty || reply="n"
  case "$reply" in
    [yY]|[yY][eE][sS])
      winget_install "$pkg" "$label" || { echo "     Manual install: $url"; return 1; }
      command -v "$cmd" >/dev/null 2>&1 || {
        err "$label still isn't on PATH in this shell."
        echo "     Close Git Bash, reopen as Administrator, and re-run ./run.sh"
        return 1
      }
      ;;
    *)
      echo "     Skipped. Install it manually: $url"
      return 1
      ;;
  esac
}

check_prerequisites() {
  info "Checking prerequisites"
  local failed=0

  prompt_install docker "Docker.DockerDesktop" "Docker Desktop" \
    "https://www.docker.com/products/docker-desktop/" || failed=1
  prompt_install go "GoLang.Go" "Go" \
    "https://go.dev/dl/" || failed=1
  prompt_install python "Python.Python.3.12" "Python 3" \
    "https://www.python.org/downloads/" || failed=1

  [ "$failed" -eq 0 ] || die "Missing prerequisites — see above."

  # docker compose v2 ships as a subcommand, not a separate binary.
  docker compose version >/dev/null 2>&1 \
    || die "'docker compose' is unavailable. Update Docker Desktop to v2+."
  ok "docker compose available"

  # Go 1.22+ is required for the ServeMux "GET /path" route patterns.
  local go_ver major minor
  go_ver="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  major="${go_ver%%.*}"; minor="$(printf '%s' "$go_ver" | cut -d. -f2)"
  if [ -n "$major" ] && { [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "${minor:-0}" -lt 22 ]; }; }; then
    die "Go $go_ver found, but 1.22+ is required (method-based route patterns)."
  fi
  ok "go $go_ver"

  command -v curl >/dev/null 2>&1 || die "curl not found (ships with Git for Windows)."
}

# --------------------------------------------------------- docker startup ---

docker_engine_up() { docker info >/dev/null 2>&1; }

start_docker_desktop() {
  if docker_engine_up; then
    ok "Docker engine is running"
    return
  fi

  info "Docker engine isn't running — starting Docker Desktop"

  local exe found=""
  for exe in \
    "/c/Program Files/Docker/Docker/Docker Desktop.exe" \
    "/c/Program Files (x86)/Docker/Docker/Docker Desktop.exe" \
    "$LOCALAPPDATA/Docker/Docker Desktop.exe"
  do
    [ -f "$exe" ] && { found="$exe"; break; }
  done

  [ -n "$found" ] || die "Docker Desktop.exe not found. Start Docker Desktop manually, then re-run."

  "$found" >/dev/null 2>&1 &
  disown 2>/dev/null || true

  info "Waiting for the Docker engine (this can take a minute on a cold start)"
  local i
  for i in $(seq 1 90); do
    if docker_engine_up; then
      ok "Docker engine ready after ${i}s"
      return
    fi
    printf '.'
    sleep 1
  done
  echo
  die "Docker engine didn't come up in 90s. Open Docker Desktop and check for errors."
}

# ------------------------------------------------------------- containers ---

container_health() {
  docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \
    "$1" 2>/dev/null || echo "missing"
}

wait_for_healthy() {
  local name="$1" timeout="${2:-90}" i status
  info "Waiting for $name to report healthy"
  for i in $(seq 1 "$timeout"); do
    status="$(container_health "$name")"
    case "$status" in
      healthy) ok "$name healthy after ${i}s"; return 0 ;;
      unhealthy)
        err "$name reported unhealthy. Recent logs:"
        docker logs --tail 30 "$name" 2>&1 | sed 's/^/       /' >&2
        return 1 ;;
    esac
    printf '.'
    sleep 1
  done
  echo
  err "$name never became healthy (waited ${timeout}s). Recent logs:"
  docker logs --tail 30 "$name" 2>&1 | sed 's/^/       /' >&2
  return 1
}

start_infrastructure() {
  info "Starting Postgres + NATS"
  docker compose up -d || die "docker compose up failed."
  wait_for_healthy food_delivery_db   90 || die "Postgres never became healthy."
  wait_for_healthy food_delivery_nats 60 || die "NATS never became healthy."
}

# ---------------------------------------------------------- port checking ---

port_in_use() {
  # netstat ships with Windows and works fine under Git Bash.
  netstat -ano 2>/dev/null | grep -E "LISTENING" | grep -q ":$1 " && return 0
  return 1
}

check_ports() {
  local p conflicts=0
  for p in 8080 8501; do
    if port_in_use "$p"; then
      err "Port $p is already in use."
      echo "     Find it with:  netstat -ano | grep :$p"
      echo "     Then stop it, or set HTTP_PORT (8080) to something else."
      conflicts=1
    fi
  done
  [ "$conflicts" -eq 0 ] || die "Free the ports above and re-run."
  ok "ports 8080 and 8501 are free"
}

# ------------------------------------------------------------- go service ---

start_service() {
  info "Building the Go service (-> $BUILD_DIR)"
  go build -o "$SERVICE_BIN" . || die "go build failed — see the errors above."

  # The binary can legitimately take a moment to actually appear even after
  # go reports success, if antivirus grabs it for a real-time scan. Poll
  # briefly before treating it as missing.
  local waited=0
  until [ -f "$SERVICE_BIN" ]; do
    waited=$((waited + 1))
    [ "$waited" -gt 20 ] && die "go build reported success but $SERVICE_BIN never appeared after 5s. Likely antivirus quarantining a freshly-built .exe — check your AV's recent/quarantined items for vaurd-service.exe and allow it, then re-run."
    sleep 0.25
  done
  ok "built $(basename "$SERVICE_BIN")"

  info "Starting the service (logs -> ${SERVICE_LOG#$REPO_ROOT/})"
  "$SERVICE_BIN" >"$SERVICE_LOG" 2>&1 &
  SERVICE_PID=$!
  pause_for_firewall_prompt "the Go service" 8080

  local i
  for i in $(seq 1 45); do
    if ! kill -0 "$SERVICE_PID" 2>/dev/null; then
      err "The service exited during startup. Log:"
      sed 's/^/       /' "$SERVICE_LOG" >&2
      die "Service failed to start."
    fi
    if curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
      ok "service healthy on :8080 (pid $SERVICE_PID)"
      return
    fi
    printf '.'
    sleep 1
  done
  echo
  err "Service didn't answer /healthz in 45s. Log:"
  sed 's/^/       /' "$SERVICE_LOG" >&2
  die "Service startup timed out."
}

# ----------------------------------------------------------- python parts ---

PY=""
pick_python() {
  local candidate
  for candidate in python python3 py; do
    if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c 'import sys; sys.exit(0 if sys.version_info>=(3,10) else 1)' 2>/dev/null; then
      PY="$candidate"
      ok "using $candidate ($("$candidate" --version 2>&1))"
      return
    fi
  done
  die "No Python 3.10+ found on PATH."
}

pip_install() {
  # Deliberately takes a path RELATIVE to $REPO_ROOT, not an absolute one.
  # The script's cwd is pinned to $REPO_ROOT for its whole run (see the `cd`
  # near the top), so a bare relative filename resolves correctly without
  # ever needing to hand a POSIX-style absolute path (e.g.
  # /c/Users/.../requirements.txt) to python.exe. Git Bash auto-translates
  # those for *some* native Windows programs but not reliably all of them —
  # go.exe gets it right, whichever python.exe ends up as $PY may not — so
  # relative paths sidestep the whole question rather than depend on it.
  local req="$1" label="$2"
  info "Installing $label dependencies"
  if ! "$PY" -m pip install --quiet --disable-pip-version-check -r "$req"; then
    warn "pip install failed for $label — retrying with --user"
    "$PY" -m pip install --quiet --disable-pip-version-check --user -r "$req" \
      || die "Could not install $label dependencies from $REPO_ROOT/$req"
  fi
  ok "$label dependencies ready"
}

start_generator() {
  pip_install "requirements.txt" "generator"
  info "Starting the event generator (logs -> ${GENERATOR_LOG#$REPO_ROOT/})"
  "$PY" -u generator/generate.py >"$GENERATOR_LOG" 2>&1 &
  GENERATOR_PID=$!
  sleep 2
  kill -0 "$GENERATOR_PID" 2>/dev/null || {
    err "Generator exited immediately. Log:"
    sed 's/^/       /' "$GENERATOR_LOG" >&2
    die "Generator failed to start."
  }
  ok "generator running (pid $GENERATOR_PID)"
}

open_browser() {
  # `start` is a cmd.exe builtin, so it has to be invoked through cmd. The
  # double slash stops MSYS rewriting /c into a Windows path.
  cmd //c start "" "$1" >/dev/null 2>&1 \
    || explorer.exe "$1" >/dev/null 2>&1 \
    || true
}

start_dashboard() {
  pip_install "dashboard/requirements.txt" "dashboard"
  info "Starting the Streamlit dashboard (logs -> ${DASHBOARD_LOG#$REPO_ROOT/})"
  API_URL="http://localhost:8080" "$PY" -m streamlit run dashboard/app.py \
    --server.port 8501 \
    --server.headless true \
    --browser.gatherUsageStats false \
    >"$DASHBOARD_LOG" 2>&1 &
  DASHBOARD_PID=$!
  pause_for_firewall_prompt "the Streamlit dashboard" 8501

  local i
  for i in $(seq 1 30); do
    kill -0 "$DASHBOARD_PID" 2>/dev/null || {
      warn "Dashboard exited during startup — continuing without it. Log:"
      sed 's/^/       /' "$DASHBOARD_LOG" >&2
      DASHBOARD_PID=""
      return
    }
    if curl -fsS http://localhost:8501 >/dev/null 2>&1; then
      ok "dashboard running on :8501 (pid $DASHBOARD_PID)"
      open_browser "http://localhost:8501"
      return
    fi
    sleep 1
  done
  warn "Dashboard didn't respond in 30s — continuing. Check ${DASHBOARD_LOG#$REPO_ROOT/}"
}

# -------------------------------------------------------------- teardown ----

teardown_and_exit() {
  info "Tearing down containers and volumes"
  docker compose down -v --remove-orphans || die "docker compose down failed."
  rm -rf "$LOG_DIR"
  ok "Everything removed. Next ./run.sh starts from a clean database."
  exit 0
}

# ------------------------------------------------------------------ main ----

print_summary() {
  cat <<EOF

${BOLD}Everything is up.${NC}

  Dashboard    http://localhost:8501$([ -z "$DASHBOARD_PID" ] && printf '   (not running)')
  API          http://localhost:8080
  NATS monitor http://localhost:8222

  Try these in another Git Bash window:

    curl "http://localhost:8080/healthz"
    curl "http://localhost:8080/orders?limit=5"
    curl "http://localhost:8080/orders?status=Preparing&sort=last_updated_asc&limit=10"

  Run the six scripted edge-case scenarios (see test.md) in another window:

    docker exec -it food_delivery_db psql -U vaurd_user -d food_delivery -c "TRUNCATE TABLE orders;"
    python generator/manual_simulation.py

  Logs:  ${LOG_DIR#$REPO_ROOT/}/

${BOLD}Press Ctrl+C to stop everything.${NC}
Streaming the service log below — look for 'applied event', 'stale/duplicate
status update ignored', and 'invalid status transition rejected'.

EOF
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --no-dashboard) RUN_DASHBOARD=0 ;;
      --no-generator) RUN_GENERATOR=0 ;;
      --skip-checks)  SKIP_CHECKS=1 ;;
      --down)         require_admin; start_docker_desktop; teardown_and_exit ;;
      -h|--help)      sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
      *)              die "Unknown flag: $1  (try --help)" ;;
    esac
    shift
  done

  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) : ;;
    *) warn "This script targets Git Bash on Windows. On Linux/macOS the admin check and winget installs won't apply." ;;
  esac

  [ -f "$REPO_ROOT/go.mod" ] || die "Run this from the repo root (go.mod not found)."

  mkdir -p "$LOG_DIR"
  trap cleanup EXIT INT TERM

  printf '\n%sVaurd order processing service — setup and demo%s\n\n' "$BOLD" "$NC"

  require_admin
  [ "$SKIP_CHECKS" -eq 1 ] || check_prerequisites
  pick_python
  check_ports
  start_docker_desktop
  start_infrastructure
  start_service
  [ "$RUN_GENERATOR" -eq 1 ] && start_generator
  [ "$RUN_DASHBOARD" -eq 1 ] && start_dashboard

  print_summary
  tail -f "$SERVICE_LOG"
}

main "$@"
