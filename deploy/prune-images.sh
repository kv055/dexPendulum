#!/usr/bin/env bash
# Remove Docker images on the VPS that neither a container nor a retained
# release can use. `docker image prune -a` is not safe here: it also deletes
# the previous release's images, which the transactional rollback restarts.
#
# Kept:
#   - the image of every container, running or stopped;
#   - every image referenced by .deploy/active.compose.json or by a retained
#     release under .deploy/releases (images.json digest pins and the legacy
#     rollback.compose.json local tags);
#   - every image tagged with a release SHA listed in a retained images.env.
# Everything else is removed. If the keep set cannot be built, nothing is.
#
# Runs on the VPS: piped over SSH by the pre-deploy job, and called by
# transactional-deploy.sh after a healthy release (with --lock-held).
#
# usage: prune-images.sh --repo-dir <path> [--dry-run] [--lock-held]
set -Eeuo pipefail

usage() {
  echo "usage: $0 --repo-dir <path> [--dry-run] [--lock-held]" >&2
  exit 2
}

repo_dir=""
dry_run=false
lock_held=false
while (($#)); do
  case "$1" in
    --repo-dir) repo_dir="${2:-}"; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    # The caller already holds .deploy/deploy.lock (transactional-deploy.sh).
    --lock-held) lock_held=true; shift ;;
    *) usage ;;
  esac
done

[[ "$repo_dir" == /* && "$repo_dir" != / ]] || usage
[[ -d "$repo_dir" ]] || {
  echo "missing repo dir $repo_dir" >&2
  exit 1
}
state_root="$repo_dir/.deploy"

# A deployment pulls its candidate images before any container uses them, so
# pruning concurrently could delete them. Skip rather than wait.
if ! "$lock_held" && [[ -d "$state_root" ]]; then
  exec 8>>"$state_root/deploy.lock"
  if ! flock -n 8; then
    echo "skipping image prune: a deployment is in progress"
    exit 0
  fi
fi

declare -A keep=()
keep_id() {
  [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "unexpected image id: $1" >&2
    return 1
  }
  keep["$1"]=1
}

containers="$(docker ps -aq)"
if [[ -n "$containers" ]]; then
  # shellcheck disable=SC2086 # one container ID per word
  container_images="$(docker inspect --format '{{.Image}}' $containers)"
  while IFS= read -r id; do
    [[ -n "$id" ]] && keep_id "$id"
  done <<<"$container_images"
fi

# Image references and release SHAs from retained release state. A malformed
# file aborts the prune: an unreadable release must not lose its images.
release_refs="$(python3 - "$state_root" <<'PY'
import json, re, sys
from pathlib import Path

root = Path(sys.argv[1])
sha = re.compile(r"[0-9a-f]{40}")

def refs(node):
    if isinstance(node, dict):
        for key, value in node.items():
            if key in ("image", "configured") and isinstance(value, str):
                yield value
            else:
                yield from refs(value)
    elif isinstance(node, list):
        for value in node:
            yield from refs(value)

files = sorted(root.glob("releases/*/images.json"))
files += sorted(root.glob("releases/*/rollback.compose.json"))
if (root / "active.compose.json").is_file():
    files.append(root / "active.compose.json")
for path in files:
    for ref in refs(json.loads(path.read_text())):
        print("ref", ref)

shas = set()
for release in root.glob("releases/*"):
    if release.is_dir() and sha.fullmatch(release.name):
        shas.add(release.name)
    manifest = release / "images.env"
    if manifest.is_file():
        for line in manifest.read_text().splitlines():
            value = line.partition("=")[2].strip()
            if sha.fullmatch(value):
                shas.add(value)
deployed = root.parent / ".deployed_sha"
if deployed.is_file() and sha.fullmatch(deployed.read_text().strip()):
    shas.add(deployed.read_text().strip())
for value in sorted(shas):
    print("sha", value)
PY
)"

declare -A keep_tag=()
while read -r kind value; do
  case "$kind" in
    ref)
      # References absent from the local cache need no protection.
      if id="$(docker image inspect --format '{{.Id}}' "$value" 2>/dev/null)"; then
        keep_id "$id"
      fi
      ;;
    sha) keep_tag["$value"]=1 ;;
  esac
done <<<"$release_refs"

images="$(docker image ls --no-trunc --format '{{.ID}} {{.Repository}}:{{.Tag}}')"
declare -A names=()
while read -r id name; do
  [[ -n "$id" ]] || continue
  [[ -n "${keep_tag[${name##*:}]:-}" ]] && keep_id "$id"
  names["$id"]+="${names[$id]:+, }$name"
done <<<"$images"

echo "Docker disk usage before unused-image prune:"
docker system df

removed=0
failed=0
for id in "${!names[@]}"; do
  [[ -n "${keep[$id]:-}" ]] && continue
  if "$dry_run"; then
    echo "would remove ${id#sha256:} (${names[$id]})"
    removed=$((removed + 1))
  # -f only removes every tag of a multi-tag image; no container uses these.
  elif docker image rm -f "$id" >/dev/null; then
    echo "removed ${id#sha256:} (${names[$id]})"
    removed=$((removed + 1))
  else
    echo "warning: could not remove ${id#sha256:} (${names[$id]})" >&2
    failed=$((failed + 1))
  fi
done

if "$dry_run"; then
  echo "dry run: $removed of ${#names[@]} images would be removed, ${#keep[@]} kept"
  exit 0
fi
echo "removed $removed of ${#names[@]} images, kept ${#keep[@]}, $failed failed"
echo "Docker disk usage after unused-image prune:"
docker system df
