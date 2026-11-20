#!/usr/bin/env bash
# Runs ON THE SERVER (copied there by .github/workflows/deploy.yml).
#
#   deploy.sh <deploy_path> <image> <tag> <registry_user>   (registry token on stdin)
#
# Pulls <image>:<tag>, restarts the stack with it, waits for /healthz, and rolls back to the
# previously deployed tag if the new one doesn't become healthy.
set -euo pipefail

path=$1 image=$2 tag=$3 user=$4
cd "$path"

IFS= read -r token || true
if [ -n "${token:-}" ]; then
  printf '%s' "$token" | docker login ghcr.io -u "$user" --password-stdin >/dev/null
  trap 'docker logout ghcr.io >/dev/null 2>&1 || true' EXIT
fi
unset token

prev=$(cat .deployed_tag 2>/dev/null || true)

up() { API_IMAGE="$image:$1" docker compose up -d --no-build --remove-orphans; }

healthy() {
  for _ in $(seq 1 30); do
    if curl -fsS -m 3 http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# If the deploy is interrupted after switching (cancelled run, dropped SSH connection), don't
# leave an unverified release running: go back to the previous one.
switched=0
# shellcheck disable=SC2329  # called by the trap below
on_abort() {
  trap - INT TERM HUP
  if [ "$switched" = 1 ] && [ -n "$prev" ] && [ "$prev" != "$tag" ]; then
    echo "Interrupted before $tag was verified; rolling back to $prev"
    up "$prev" || true
  fi
  exit 130
}
trap on_abort INT TERM HUP

echo "Deploying $image:$tag (previous: ${prev:-none})"
API_IMAGE="$image:$tag" docker compose pull api
switched=1
up "$tag"

# Remember the live image in .env, so a manual "docker compose up -d" or a reboot keeps running it.
pin() {
  if grep -q '^API_IMAGE=' .env; then
    sed -i.bak "s|^API_IMAGE=.*|API_IMAGE=$image:$1|" .env && rm -f .env.bak
  else
    printf '\nAPI_IMAGE=%s\n' "$image:$1" >> .env
  fi
}

if healthy; then
  switched=0
  echo "$tag" > .deployed_tag
  pin "$tag"
  docker image prune -f >/dev/null || true
  echo "Healthy: $tag is live"
  exit 0
fi

switched=0 # handled below
echo "::error::$tag failed the health check. Last API logs:"
docker compose logs --tail 80 api || true
if [ -n "$prev" ] && [ "$prev" != "$tag" ]; then
  echo "Rolling back to $prev"
  up "$prev"
  if healthy; then
    echo "Rolled back to $prev"
  else
    echo "::error::Rollback to $prev is unhealthy too"
  fi
fi
exit 1
