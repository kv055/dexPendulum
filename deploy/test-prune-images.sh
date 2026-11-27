#!/usr/bin/env bash
# Exercises the rollback-safe VPS image prune against a fake docker CLI.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pruner="$repo_root/deploy/prune-images.sh"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

id() { printf 'sha256:%064d' "$1"; }
active_sha="$(printf 'a%.0s' {1..40})"
previous_sha="$(printf 'b%.0s' {1..40})"
inherited_sha="$(printf 'c%.0s' {1..40})"
trimmed_sha="$(printf 'd%.0s' {1..40})"
digest="sha256:$(printf 'e%.0s' {1..64})"

repo="$fixture/repo"
mkdir -p "$repo/.deploy/releases/$active_sha" "$repo/.deploy/releases/$previous_sha" "$fixture/bin"
printf '%s\n' "$active_sha" >"$repo/.deployed_sha"
# The active release inherits an unchanged service image from an older SHA.
printf 'DEXMONITOR_IMAGE_TAG=%s\nUNIFIED_API_IMAGE_TAG=%s\n' "$active_sha" "$inherited_sha" \
  >"$repo/.deploy/releases/$active_sha/images.env"
cat >"$repo/.deploy/releases/$previous_sha/images.json" <<EOF
{"release": "$previous_sha", "services": {"dexmonitor": {"image": "ghcr.io/t/dexarbtrading-dexmonitor@$digest", "configured": "ghcr.io/t/dexarbtrading-dexmonitor:$previous_sha"}}, "version": 1}
EOF
printf '{"services": {"tor": {"image": "dexarb-rollback/tor:%s", "pull_policy": "never"}}}\n' \
  "$previous_sha" >"$repo/.deploy/releases/$previous_sha/rollback.compose.json"

# Image IDs: 1 running, 2 stopped container, 3 previous digest pin, 4 legacy
# rollback tag, 5 inherited tag, 6 multi-tag garbage, 7 trimmed release, 8 dangling.
cat >"$fixture/images" <<EOF
$(id 1) ghcr.io/t/dexarbtrading-dexmonitor:<none>
$(id 2) postgres:16
$(id 3) ghcr.io/t/dexarbtrading-dexmonitor:<none>
$(id 4) dexarb-rollback/tor:$previous_sha
$(id 5) ghcr.io/t/dexarbtrading-unified-api:$inherited_sha
$(id 6) ghcr.io/t/dexarbtrading-unified-api:latest
$(id 6) ghcr.io/t/dexarbtrading-unified-api:old
$(id 7) ghcr.io/t/dexarbtrading-dexmonitor:$trimmed_sha
$(id 8) <none>:<none>
EOF

cat >"$fixture/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'ps -aq') printf 'c1\nc2\n' ;;
  'inspect --format {{.Image}} c1 c2') printf '%s\n%s\n' "$(printf 'sha256:%064d' 1)" "$(printf 'sha256:%064d' 2)" ;;
  'image inspect --format {{.Id}} '*)
    ref="${*: -1}"
    case "$ref" in
      *@sha256:*) printf 'sha256:%064d\n' 3 ;;
      dexarb-rollback/tor:*) printf 'sha256:%064d\n' 4 ;;
      *) echo "No such image: $ref" >&2; exit 1 ;;
    esac
    ;;
  'image ls --no-trunc --format {{.ID}} {{.Repository}}:{{.Tag}}') cat "$FAKE_IMAGES" ;;
  'system df') echo 'TYPE TOTAL ACTIVE SIZE RECLAIMABLE' ;;
  'image rm -f '*) printf '%s\n' "${*: -1}" >>"$FAKE_REMOVED" ;;
  *) echo "unexpected docker call: $*" >&2; exit 99 ;;
esac
EOF
chmod +x "$fixture/bin/docker"
export PATH="$fixture/bin:$PATH" FAKE_IMAGES="$fixture/images" FAKE_REMOVED="$fixture/removed"

removed() { sort -u "$FAKE_REMOVED" 2>/dev/null | tr '\n' ' '; }
expected="$(id 6) $(id 7) $(id 8) "

echo "== dry run removes nothing =="
bash "$pruner" --repo-dir "$repo" --dry-run | tee "$fixture/out"
[[ ! -e "$FAKE_REMOVED" ]]
grep -q 'dry run: 3 of 8 images would be removed' "$fixture/out"

echo "== a running deployment blocks the prune =="
exec 7>>"$repo/.deploy/deploy.lock"
flock -n 7
bash "$pruner" --repo-dir "$repo" | grep -q 'skipping image prune'
[[ ! -e "$FAKE_REMOVED" ]]
echo "== the deployment holding the lock may prune =="
bash "$pruner" --repo-dir "$repo" --lock-held >/dev/null
[[ "$(removed)" == "$expected" ]] || { echo "removed: $(removed)" >&2; exit 1; }
exec 7>&-
rm -f "$FAKE_REMOVED"

echo "== keeps containers and retained releases, removes the rest =="
bash "$pruner" --repo-dir "$repo" | tee "$fixture/out"
[[ "$(removed)" == "$expected" ]] || { echo "removed: $(removed)" >&2; exit 1; }
grep -q 'removed 3 of 8 images' "$fixture/out"
rm -f "$FAKE_REMOVED"

echo "== an unreadable release aborts before removing anything =="
printf '{not json' >"$repo/.deploy/releases/$previous_sha/images.json"
if bash "$pruner" --repo-dir "$repo" >/dev/null 2>&1; then
  echo "expected malformed release state to abort the prune" >&2
  exit 1
fi
[[ ! -e "$FAKE_REMOVED" ]]

echo "== rejects a relative repo dir =="
if bash "$pruner" --repo-dir relative >/dev/null 2>&1; then
  echo "expected a relative repo dir to be rejected" >&2
  exit 1
fi

echo "image prune tests passed"
