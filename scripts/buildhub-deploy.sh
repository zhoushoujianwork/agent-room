#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"

SERVICE="${BUILDHUB_SERVICE:-agent-room}"
SERVICE_ID="${BUILDHUB_SERVICE_ID:-3cb98176-df65-46d4-b80a-95d89516c845}"
VM_SSH_USER="${BUILDHUB_VM_SSH_USER:-ubuntu}"
VM_IP="${BUILDHUB_VM_IP:-10.150.115.200}"
PORT="${BUILDHUB_BACKEND_PORT:-31080}"
DOMAIN="${BUILDHUB_DOMAIN:-https://agent-room-zhoushoujian.build.patsnap.info}"
RUNTIME="${BUILDHUB_RUNTIME:-go1.26.3-linux-amd64}"
ACCESS_MODE="${BUILDHUB_ACCESS_MODE:-intranet}"
DESCRIPTION="${BUILDHUB_DESCRIPTION:-隔离桌面 AI Agent 的聊天桥接平台，支持房间协作、权限审批、远程执行、临时 skill 授权和 Direct AI 对话接入}"
BACKEND_CMD='agent-room relay -addr :$PORT -db ~/apps/agent-room/shared/db/agent-room.db'
TAGS=(llm chat agent)

skip_tests=false
skip_frontend=false
dry_run=false
require_clean=false
label=""

usage() {
  cat <<'USAGE'
Usage: scripts/buildhub-deploy.sh [options]

Build and deploy Agent Room to Build Hub without interactive confirmation.
The script builds the frontend, runs Go tests, cross-compiles a Linux binary,
uploads a versioned release to the Build Hub VM, restarts systemd, registers
metadata, and verifies /healthz.

Options:
  --label NAME             Append NAME to the release version.
  --skip-tests             Skip go test ./...
  --skip-frontend-build    Skip npm run build in frontend/.
  --require-clean          Fail if git has uncommitted changes.
  --dry-run                Print the deployment plan and exit.
  -h, --help               Show this help.

Environment overrides:
  BUILDHUB_SERVICE, BUILDHUB_SERVICE_ID, BUILDHUB_VM_SSH_USER, BUILDHUB_VM_IP,
  BUILDHUB_BACKEND_PORT, BUILDHUB_DOMAIN, BUILDHUB_RUNTIME, BUILDHUB_ACCESS_MODE.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --label)
      label="${2:-}"
      shift 2
      ;;
    --skip-tests)
      skip_tests=true
      shift
      ;;
    --skip-frontend-build)
      skip_frontend=true
      shift
      ;;
    --require-clean)
      require_clean=true
      shift
      ;;
    --dry-run)
      dry_run=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

log() {
  printf '\n[%s] %s\n' "$(TZ=Asia/Shanghai date '+%H:%M:%S')" "$*"
}

fail() {
  echo "buildhub deploy failed: $*" >&2
  exit 1
}

on_error() {
  local exit_code=$?
  local line_no=$1
  set +e
  echo >&2
  echo "buildhub deploy failed at line ${line_no} (exit ${exit_code})." >&2
  echo "Recent ${SERVICE}.service logs:" >&2
  ssh -o BatchMode=yes -o ConnectTimeout=5 "${VM_SSH_USER}@${VM_IP}" \
    "journalctl --user -u ${SERVICE}.service -n 80 --no-pager" >&2 || true
  echo >&2
  echo "Rollback hint:" >&2
  echo "  ssh ${VM_SSH_USER}@${VM_IP} 'ls -1dt ~/apps/${SERVICE}/releases/* | head -5'" >&2
  echo "  ssh ${VM_SSH_USER}@${VM_IP} 'ln -sfn ~/apps/${SERVICE}/releases/<version> ~/apps/${SERVICE}/current && systemctl --user restart ${SERVICE}.service'" >&2
  exit "$exit_code"
}
trap 'on_error $LINENO' ERR

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

sanitize_label() {
  printf '%s' "$1" \
    | tr '[:upper:]' '[:lower:]' \
    | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//; s/-+/-/g' \
    | cut -c1-32
}

find_buildhub_scripts() {
  local candidates=(
    "${CLAUDE_PLUGIN_ROOT:-}/scripts"
    "${HOME}/.claude/plugins/build-hub/scripts"
    "${HOME}/.claude/skills/build-hub/scripts"
    "${HOME}/.codex/plugins/build-hub/scripts"
  )
  local c
  for c in "${candidates[@]}"; do
    [[ -n "$c" && -x "${c}/register.sh" ]] && printf '%s\n' "$c" && return 0
  done
  if [[ -d "${HOME}/.codex/plugins/cache" ]]; then
    c="$(find "${HOME}/.codex/plugins/cache" -path '*/build-hub/*/scripts/register.sh' -print -quit 2>/dev/null || true)"
    if [[ -n "$c" ]]; then
      dirname "$c"
      return 0
    fi
  fi
  return 1
}

ensure_buildhub_login() {
  if [[ -s "${HOME}/.claude/build-hub/token" ]]; then
    return
  fi
  log "Build Hub token missing; starting device login"
  bash "${SCRIPTS}/login.sh"
}

ensure_buildhub_keys() {
  local marker="${HOME}/.claude/build-hub/keys-uploaded"
  if [[ -f "$marker" ]]; then
    return
  fi
  log "Uploading SSH public key to Build Hub"
  bash "${SCRIPTS}/keys.sh"
  mkdir -p "$(dirname "$marker")"
  touch "$marker"
}

ensure_vm() {
  if ssh -o BatchMode=yes -o ConnectTimeout=5 "${VM_SSH_USER}@${VM_IP}" "true" >/dev/null 2>&1; then
    return
  fi

  log "Cached VM ${VM_IP} is not reachable; asking Build Hub for VM info"
  local vm_cache="${HOME}/.claude/build-hub/vm.env"
  mkdir -p "$(dirname "$vm_cache")"
  bash "${SCRIPTS}/init-vm.sh" 2>/dev/null | grep -E '^VM_(IP|SSH_USER|STATUS)=' > "$vm_cache"
  # shellcheck disable=SC1090
  . "$vm_cache"
  VM_IP="${VM_IP:?}"
  VM_SSH_USER="${VM_SSH_USER:?}"
  ssh -o BatchMode=yes -o ConnectTimeout=10 "${VM_SSH_USER}@${VM_IP}" "true"
}

git_dirty() {
  [[ -n "$(git status --porcelain --untracked-files=normal)" ]]
}

build_version() {
  local short dirty suffix clean_label
  short="$(git rev-parse --short=7 HEAD 2>/dev/null || echo nogit)"
  dirty=""
  if git_dirty; then
    dirty="-dirty"
  fi
  suffix=""
  clean_label="$(sanitize_label "$label")"
  if [[ -n "$clean_label" ]]; then
    suffix="-${clean_label}"
  fi
  printf '%s-%s%s%s' "$DEPLOY_TS" "$short" "$dirty" "$suffix"
}

update_deploy_record() {
  local record="${repo_root}/.build-hub/${SERVICE}.md"
  [[ -f "$record" ]] || return 0
  local deployed_at tmp
  deployed_at="$(TZ=Asia/Shanghai date '+%Y-%m-%d %H:%M Asia/Shanghai')"
  tmp="$(mktemp)"
  awk -v version="$VERSION" -v deployed_at="$deployed_at" '
    /^- Last deployed version:/ { print "- Last deployed version: `" version "`"; next }
    /^- Last deployed at:/ { print "- Last deployed at: `" deployed_at "`"; next }
    { print }
  ' "$record" > "$tmp"
  mv "$tmp" "$record"
}

register_buildhub_service() {
  bash "${SCRIPTS}/register.sh" \
    --id "$SERVICE_ID" \
    --runtime "$RUNTIME" \
    --description "$DESCRIPTION" \
    --backend-port "$PORT" \
    --backend-cmd "$BACKEND_CMD" \
    --access-mode "$ACCESS_MODE" \
    --tag "${TAGS[0]}" \
    --tag "${TAGS[1]}" \
    --tag "${TAGS[2]}"
}

require_cmd git
require_cmd go
if [[ "$skip_frontend" != "true" ]]; then
  require_cmd npm
fi
require_cmd ssh
require_cmd scp
require_cmd tar
require_cmd curl
require_cmd awk
require_cmd sed

cd "$repo_root"

if [[ "$require_clean" == "true" ]] && git_dirty; then
  fail "git working tree is dirty; commit/stash changes or omit --require-clean"
fi

SCRIPTS="$(find_buildhub_scripts)" || fail "could not find Build Hub scripts"
DEPLOY_TS="$(TZ=Asia/Shanghai date +%Y%m%d-%H%M%S)"
VERSION="$(build_version)"
BASE_DIR="apps/${SERVICE}"
REMOTE="${VM_SSH_USER}@${VM_IP}"
TARBALL="${TMPDIR:-/tmp}/${SERVICE}-${VERSION}.tar.gz"
LOCAL_BIN="${TMPDIR:-/tmp}/${SERVICE}-${VERSION}-linux-amd64"
REMOTE_BIN="${BASE_DIR}/packages/${SERVICE}-${VERSION}-linux-amd64"

cat <<PLAN
Build Hub deploy plan
  service:      ${SERVICE}
  version:      ${VERSION}
  vm:           ${REMOTE}
  port:         ${PORT}
  domain:       ${DOMAIN}
  scripts:      ${SCRIPTS}
  tests:        $([[ "$skip_tests" == "true" ]] && echo skip || echo run)
  frontend:     $([[ "$skip_frontend" == "true" ]] && echo skip || echo build)
PLAN

if [[ "$dry_run" == "true" ]]; then
  exit 0
fi

ensure_buildhub_login
ensure_buildhub_keys
ensure_vm
REMOTE="${VM_SSH_USER}@${VM_IP}"

log "Checking Build Hub AI-ready issues"
if issues_json="$(bash "${SCRIPTS}/issues.sh" ai-ready --id "$SERVICE_ID" 2>/dev/null)"; then
  issue_count="$(printf '%s' "$issues_json" | { grep -o '"id"' || true; } | wc -l | tr -d ' ')"
  if [[ "${issue_count:-0}" != "0" ]]; then
    echo "Found ${issue_count} AI-ready issue(s); deploy continues without auto-fixing issues."
  fi
fi

if [[ "$skip_frontend" != "true" ]]; then
  log "Building frontend"
  (cd frontend && npm run build)
  VERSION="$(build_version)"
  TARBALL="${TMPDIR:-/tmp}/${SERVICE}-${VERSION}.tar.gz"
  LOCAL_BIN="${TMPDIR:-/tmp}/${SERVICE}-${VERSION}-linux-amd64"
  REMOTE_BIN="${BASE_DIR}/packages/${SERVICE}-${VERSION}-linux-amd64"
  log "Release version after frontend build: ${VERSION}"
fi

if [[ "$skip_tests" != "true" ]]; then
  log "Running Go tests"
  go test ./...
fi

log "Cross-compiling Linux binary"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w -X agent-room/internal/version.Version=${VERSION}" \
  -o "$LOCAL_BIN" .

log "Creating release tarball"
rm -f "$TARBALL"
tar -czf "$TARBALL" \
  --exclude './.git' \
  --exclude './.build-hub' \
  --exclude './.claude' \
  --exclude './frontend/node_modules' \
  --exclude './node_modules' \
  --exclude './deploy-stage' \
  --exclude './output' \
  --exclude './tmp' \
  --exclude './videos' \
  --exclude './agent-room' \
  --exclude './frontend/tsconfig.tsbuildinfo' \
  --exclude '*.db' \
  --exclude '*.db-shm' \
  --exclude '*.db-wal' \
  --exclude '.DS_Store' \
  --exclude './internal/api/relay/assets/downloads/agent-room-*' \
  .

log "Uploading release package and binary"
ssh "$REMOTE" "mkdir -p ~/${BASE_DIR}/packages ~/${BASE_DIR}/releases ~/${BASE_DIR}/shared/db ~/${BASE_DIR}/shared/backups"
scp "$TARBALL" "$REMOTE:${BASE_DIR}/packages/"
scp "$LOCAL_BIN" "$REMOTE:${REMOTE_BIN}"

log "Installing release and restarting systemd"
ssh "$REMOTE" \
  "SERVICE='$SERVICE' VERSION='$VERSION' PORT='$PORT' BACKEND_CMD='$BACKEND_CMD' bash -s" <<'REMOTE_SCRIPT'
set -euo pipefail

BASE_DIR="$HOME/apps/$SERVICE"
PACKAGE="$BASE_DIR/packages/$SERVICE-$VERSION.tar.gz"
RELEASE_DIR="$BASE_DIR/releases/$VERSION"
REMOTE_BIN="$BASE_DIR/packages/$SERVICE-$VERSION-linux-amd64"

if [[ -x "$BASE_DIR/shared/backup.sh" ]]; then
  "$BASE_DIR/shared/backup.sh"
fi

rm -rf "$RELEASE_DIR"
mkdir -p "$RELEASE_DIR"
tar -xzf "$PACKAGE" -C "$RELEASE_DIR"
mkdir -p "$RELEASE_DIR/bin"
cp "$REMOTE_BIN" "$RELEASE_DIR/bin/agent-room"
chmod +x "$RELEASE_DIR/bin/agent-room"

mkdir -p "$HOME/.config/systemd/user"
if ! systemctl --user cat "$SERVICE.service" >/dev/null 2>&1; then
  cat > "$HOME/.config/systemd/user/$SERVICE.service" <<UNIT
[Unit]
Description=agent-room relay (build-hub)
[Service]
WorkingDirectory=%h/apps/agent-room/current
Environment=PORT=$PORT
EnvironmentFile=-%h/apps/agent-room/shared/.env.buildhub
ExecStart=%h/apps/agent-room/current/bin/agent-room relay -addr :\${PORT} -db %h/apps/agent-room/shared/db/agent-room.db
Restart=always
[Install]
WantedBy=default.target
UNIT
fi

ln -sfn "$RELEASE_DIR" "$BASE_DIR/current"
systemctl --user daemon-reload
systemctl --user enable --now "$SERVICE.service" >/dev/null
systemctl --user restart "$SERVICE.service"
systemctl --user is-active --quiet "$SERVICE.service"

cd "$BASE_DIR/releases" && ls -1dt */ 2>/dev/null | tail -n +6 | xargs -r rm -rf
cd "$BASE_DIR/packages" && ls -1t "$SERVICE"-*.tar.gz 2>/dev/null | tail -n +6 | xargs -r rm -f
REMOTE_SCRIPT

log "Verifying direct health"
curl -fsS "http://${VM_IP}:${PORT}/healthz"
echo

log "Registering Build Hub metadata"
register_buildhub_service

log "Verifying Build Hub domain"
curl -fsS "${DOMAIN}/healthz"
echo

update_deploy_record

cat <<DONE

Deploy complete
  service: ${SERVICE}
  version: ${VERSION}
  domain:  ${DOMAIN}
  health:  ${DOMAIN}/healthz
DONE
