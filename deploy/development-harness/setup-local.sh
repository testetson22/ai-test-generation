#!/usr/bin/env bash
# Prepare the local development harness: postgres + GitLab (compose), worker
# images, and GitLab seeding with the differential pair subjects. Idempotent.
#
# Prerequisites: podman with a machine of at least 8 GiB RAM, podman-compose.
# Environment: GITLAB_ROOT_PASSWORD (default is a local-only convenience),
#              ATG_SKIP_SEED=1 to skip GitLab seeding.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
HARNESS="$REPO_ROOT/deploy/development-harness"
GITLAB_URL="http://127.0.0.1:8090"
WORKER_REMOTE="http://host.containers.internal:8090/development/spring-boot-example-application.git"
BASE_COMMIT="83b6f931c08071133678284548ff19b13f6f8db0"
FIX_COMMIT="518440bd30e5a2b5760861e7709e83963461fe8f"
UPSTREAM="https://github.com/lzspangler/spring-boot-example-application"
PAT="atg-local-pat-20261124"
TOKEN_TTL_DAYS=90

log() { printf '[setup-local] %s\n' "$*"; }

memory_mib="$(podman info --format '{{.Host.Memory}}' 2>/dev/null || echo 0)"
if [ "${memory_mib:-0}" -lt 7000000 ]; then
  log "ERROR: podman machine memory is ${memory_mib} bytes; GitLab needs ~8 GiB."
  log "Run: podman machine stop && podman machine set --memory 8192 && podman machine start"
  exit 1
fi

log "starting postgres and GitLab (compose)"
(cd "$HARNESS" && podman-compose up -d)

log "waiting for postgres"
for _ in $(seq 1 60); do
  if podman exec atg-local-development-postgres pg_isready -U atg -d atg >/dev/null 2>&1; then break; fi
  sleep 2
done
podman exec atg-local-development-postgres pg_isready -U atg -d atg >/dev/null

log "waiting for GitLab (first boot can take several minutes)"
for _ in $(seq 1 120); do
  if curl -sf "$GITLAB_URL/users/sign_in" >/dev/null 2>&1; then break; fi
  sleep 10
done
curl -sf "$GITLAB_URL/users/sign_in" >/dev/null

log "building worker images"
podman build --pull=never -t localhost/atg-java17-maven-v3:local \
  -f "$REPO_ROOT/workers/java17-maven/Containerfile" "$REPO_ROOT/workers/java17-maven"
podman build --pull=never \
  --build-arg PYTHON_BASE=docker.io/library/python@sha256:935c3c5b12357a7e10caa9c1e6ca5692344d97cf229832ef584be2cec9fc03df \
  --build-arg HARNESS=pydantic_ai \
  -t localhost/atg-generation-pydantic-v6:local \
  -f "$REPO_ROOT/workers/generation/Containerfile" "$REPO_ROOT"

if [ "${ATG_SKIP_SEED:-0}" = "1" ]; then
  log "ATG_SKIP_SEED=1; skipping GitLab seeding"
  exit 0
fi

log "ensuring root access token"
if ! curl -sf -H "PRIVATE-TOKEN: $PAT" "$GITLAB_URL/api/v4/user" >/dev/null 2>&1; then
  podman exec atg-gitlab gitlab-rails runner "t = User.find_by_username('root').personal_access_tokens.create(name: 'atg-local-dev', scopes: [:api, :write_repository], expires_at: $TOKEN_TTL_DAYS.days.from_now); t.set_token('$PAT'); t.save!; puts 'created'" 2>&1 | grep -v DEPRECATION | tail -1
fi
curl -sf -H "PRIVATE-TOKEN: $PAT" "$GITLAB_URL/api/v4/user" >/dev/null

log "ensuring development group and project"
curl -sf -X POST "$GITLAB_URL/api/v4/groups" -H "PRIVATE-TOKEN: $PAT" \
  -H 'Content-Type: application/json' \
  --data-raw '{"path":"development","name":"development","visibility":"public"}' >/dev/null || true
group_id="$(curl -sf -H "PRIVATE-TOKEN: $PAT" "$GITLAB_URL/api/v4/groups/development" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
curl -sf -X POST "$GITLAB_URL/api/v4/projects" -H "PRIVATE-TOKEN: $PAT" \
  -H 'Content-Type: application/json' \
  --data-raw "{\"path\":\"spring-boot-example-application\",\"name\":\"spring-boot-example-application\",\"namespace_id\":$group_id,\"visibility\":\"public\",\"default_branch\":\"main\"}" >/dev/null || true

log "importing pair subjects (deterministic fix commit)"
SEED="$(mktemp -d)"
trap 'rm -rf "$SEED"' EXIT
git clone --quiet "$UPSTREAM" "$SEED/repo"
git -C "$SEED/repo" checkout --quiet "$BASE_COMMIT"
git -C "$SEED/repo" config user.name "ATG fix candidate"
git -C "$SEED/repo" config user.email "atg@localhost"
export GIT_AUTHOR_DATE="2026-11-24T14:57:58-05:00"
export GIT_COMMITTER_DATE="2026-11-24T14:57:58-05:00"
if [ "$(git -C "$SEED/repo" rev-parse HEAD)" != "$FIX_COMMIT" ]; then
  sed -i.bak 's|<version>6.0.3</version>|<version>6.4.0</version>|' "$SEED/repo/pom.xml" && rm -f "$SEED/repo/pom.xml.bak"
  git -C "$SEED/repo" add pom.xml
  git -C "$SEED/repo" commit --quiet -m "Bump woodstox-core to 6.4.0 (CVE-2022-40152 fix candidate)"
fi
if [ "$(git -C "$SEED/repo" rev-parse HEAD)" != "$FIX_COMMIT" ]; then
  log "ERROR: fix commit did not reproduce $FIX_COMMIT (got $(git -C "$SEED/repo" rev-parse HEAD))"
  exit 1
fi
git -C "$SEED/repo" remote add local "http://root:${PAT}@127.0.0.1:8090/development/spring-boot-example-application.git"
git -C "$SEED/repo" push -q local "${BASE_COMMIT}:refs/heads/main" "${FIX_COMMIT}:refs/heads/fix-candidate"

log "verifying worker-network access to the pair"
podman run --rm docker.io/library/maven@sha256:7d92f4b97ceca94699c70dfe41e186115de1a9e34dd50413ab3f96ce32bc60ec \
  git ls-remote "$WORKER_REMOTE" "refs/heads/fix-candidate" | grep -q "$FIX_COMMIT"

log "harness ready"
log "  postgres: 127.0.0.1:55432 (atg/atg-local-only)"
log "  gitlab:   $GITLAB_URL (root / \$GITLAB_ROOT_PASSWORD)"
log "  pair:     base=$BASE_COMMIT candidate=$FIX_COMMIT at $WORKER_REMOTE"
log "start the service with ATG_LOCAL_PROFILE=$REPO_ROOT/workers/java17-maven/profile-unit-pair-cve-online.json"
