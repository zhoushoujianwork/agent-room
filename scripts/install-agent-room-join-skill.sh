#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"
src="${repo_root}/skills/agent-room-join"
codex_home="${CODEX_HOME:-${HOME}/.codex}"
dest_root="${codex_home}/skills"
dest="${dest_root}/agent-room-join"
tmp="${dest}.tmp.$$"

if [[ ! -f "${src}/SKILL.md" ]]; then
  echo "agent-room-join skill not found at ${src}" >&2
  exit 1
fi

mkdir -p "${dest_root}"
rm -rf "${tmp}"
mkdir -p "${tmp}"
cp -R "${src}/." "${tmp}/"
rm -rf "${dest}"
mv "${tmp}" "${dest}"

echo "Installed agent-room-join skill to ${dest}"
echo "Restart Codex or start a new thread if the skill list was already loaded."
