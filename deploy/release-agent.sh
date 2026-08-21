#!/usr/bin/env bash
#
# Installs the newest published release, and nothing else.
#
# Pull-based on purpose: the host reaches out to GitHub, so shipping never
# depends on GitHub being able to reach the host. A poll that does not happen
# costs one interval; an inbound hook that does not arrive costs the deploy.
#
# Requires: docker (with compose), curl, jq.
set -euo pipefail

REPO="${ASSET_SERVICE_REPO:-basicallysource/asset-service}"
DIR="${ASSET_SERVICE_DIR:-/opt/asset-service}"
STATE="${ASSET_SERVICE_STATE:-/var/lib/asset-service/installed-release}"
HEALTH_URL="${ASSET_SERVICE_HEALTH_URL:-http://127.0.0.1:8080/readyz}"
HEALTH_ATTEMPTS="${ASSET_SERVICE_HEALTH_ATTEMPTS:-30}"
IMAGE_KEEP_HOURS="${ASSET_SERVICE_IMAGE_KEEP_HOURS:-168}"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
compose() { docker compose --project-directory "$DIR" --file "$DIR/docker-compose.yml" "$@"; }

mkdir -p "$(dirname "$STATE")"

# One install at a time. The timer fires more often than a slow pull finishes,
# and two of these running at once would fight over the same .env.
exec 9>"${STATE}.lock"
if ! flock -n 9; then
    log "another install is already running"
    exit 0
fi

release=$(curl -fsSL --max-time 30 -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/${REPO}/releases/latest")
version=$(jq -r '.tag_name // empty' <<<"$release")
manifest_url=$(jq -r '.assets[]? | select(.name == "release.json") | .browser_download_url' <<<"$release")

if [ -z "$version" ] || [ -z "$manifest_url" ]; then
    log "no installable release found for ${REPO}"
    exit 1
fi

installed=$(cat "$STATE" 2>/dev/null || true)
if [ "$version" = "$installed" ]; then
    exit 0
fi

manifest=$(curl -fsSL --max-time 30 "$manifest_url")
image=$(jq -r '.image' <<<"$manifest")
digest=$(jq -r '.digest' <<<"$manifest")
if [ -z "$image" ] || [ -z "$digest" ] || [ "$digest" = null ]; then
    log "release ${version} does not name an image digest"
    exit 1
fi
ref="${image}@${digest}"

log "installing ${version} (${ref}); currently ${installed:-none}"

# Pull first: a failed download should change nothing about what is running.
docker pull --quiet "$ref"

previous=$(sed -n 's/^ASSET_SERVICE_IMAGE=//p' "$DIR/.env" 2>/dev/null || true)
printf 'ASSET_SERVICE_IMAGE=%s\n' "$ref" > "$DIR/.env"
compose up -d --remove-orphans

healthy=false
for _ in $(seq 1 "$HEALTH_ATTEMPTS"); do
    if curl -fsS --max-time 5 -o /dev/null "$HEALTH_URL"; then
        healthy=true
        break
    fi
    sleep 2
done

if [ "$healthy" != true ]; then
    log "${version} did not become healthy"
    if [ -n "$previous" ]; then
        log "rolling back to ${previous}"
        printf 'ASSET_SERVICE_IMAGE=%s\n' "$previous" > "$DIR/.env"
        compose up -d --remove-orphans
    fi
    exit 1
fi

printf '%s\n' "$version" > "$STATE"
log "installed ${version}"

# Old images are the largest thing a deploy leaves behind. The filters keep
# this to unused images of this service alone.
docker image prune --all --force \
    --filter "until=${IMAGE_KEEP_HOURS}h" \
    --filter "label=org.opencontainers.image.title=asset-service" >/dev/null || true
