#!/usr/bin/env bash
# Build gtb, push it to the nhaboc Nexus registry and (re)deploy it to k3s.
# Run on the k3s host (beboc) as a user with sudo, docker and kubectl:
#   curl -fsSL https://raw.githubusercontent.com/baovo15/gtb/main/scripts/gtb-deploy.sh | bash
# Re-running it is a redeploy: it pulls main, builds a new tag and rolls the
# gtb StatefulSet. The town data under /var/lib/gtb survives.
set -euo pipefail

REPO_URL=${REPO_URL:-https://github.com/baovo15/gtb.git}
SRC=${SRC:-$HOME/src/gtb}
REGISTRY=${REGISTRY:-127.0.0.1:30082}
NEXUS_USER=${NEXUS_USER:-ci}
NEXUS_PASSWORD_FILE=${NEXUS_PASSWORD_FILE:-/etc/tieudidi/nexus-ci.password}
NS=gtb

step() { printf '\n==> %s\n' "$*"; }

step "Source: $REPO_URL -> $SRC"
if [ -d "$SRC/.git" ]; then
  git -C "$SRC" fetch -q origin && git -C "$SRC" checkout -q main && git -C "$SRC" reset -q --hard origin/main
else
  git clone -q "$REPO_URL" "$SRC"
fi
TAG=$(git -C "$SRC" rev-parse --short HEAD)
IMAGE="$REGISTRY/gtb:$TAG"

step "Host volumes /var/lib/gtb (uid 1000, like gastown)"
# The pod mounts these as subPaths and runs as uid 1000; kubelet would create
# missing ones as root, and `gt install` then cannot write /gt.
sudo mkdir -p /var/lib/gtb/dolt /var/lib/gtb/town /var/lib/gtb/claude /var/lib/gtb/gt-home /var/lib/gtb/gt-state
sudo chown 1000:1000 /var/lib/gtb /var/lib/gtb/town /var/lib/gtb/claude /var/lib/gtb/gt-home /var/lib/gtb/gt-state

step "Build $IMAGE"
docker build --platform linux/amd64 -t "$IMAGE" -t "$REGISTRY/gtb:latest" "$SRC"

step "Push to Nexus ($REGISTRY, user $NEXUS_USER)"
sudo cat "$NEXUS_PASSWORD_FILE" | docker login "$REGISTRY" -u "$NEXUS_USER" --password-stdin >/dev/null
docker push -q "$IMAGE"
docker push -q "$REGISTRY/gtb:latest"

step "Namespace and secrets (copied from gastown on first deploy)"
kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS"
for pair in gastown-git:gtb-git gastown-claude-env:gtb-claude-env; do
  from=${pair%%:*} to=${pair##*:}
  if ! kubectl -n "$NS" get secret "$to" >/dev/null 2>&1; then
    kubectl -n gastown get secret "$from" -o json \
      | jq --arg n "$to" --arg ns "$NS" '{apiVersion, kind, type, data, metadata: {name: $n, namespace: $ns}}' \
      | kubectl apply -f -
  fi
done

step "Apply manifests"
sed "s|IMAGE_TAG|$TAG|" "$SRC/deploy/k3s/gtb.yaml" | kubectl apply -f -
kubectl -n "$NS" rollout status sts/gtb-dolt --timeout=5m
kubectl -n "$NS" rollout status sts/gtb --timeout=10m

step "Edge: gtb.nhaboc.com -> 127.0.0.1:30881"
# Clone the gastown vhost the first time; nginx config is host-owned, so
# anything unexpected is reported rather than guessed.
src_conf=$(sudo grep -rl 'gastown.nhaboc.com' /etc/nginx 2>/dev/null | head -1 || true)
if sudo grep -rqs 'gtb.nhaboc.com' /etc/nginx; then
  echo "nginx already serves gtb.nhaboc.com"
elif [ -n "$src_conf" ] && [ "$(sudo grep -c server_name "$src_conf")" -eq 1 ]; then
  dst_conf=$(dirname "$src_conf")/gtb.conf
  sudo sed -e 's/gastown\.nhaboc\.com/gtb.nhaboc.com/g' -e 's/30880/30881/g' "$src_conf" | sudo tee "$dst_conf" >/dev/null
  [ -d /etc/nginx/sites-enabled ] && [ "$(dirname "$dst_conf")" = /etc/nginx/sites-available ] \
    && sudo ln -sf "$dst_conf" /etc/nginx/sites-enabled/gtb.conf
  sudo nginx -t && sudo systemctl reload nginx
  echo "wrote $dst_conf from $src_conf"
else
  echo "MANUAL: no single-vhost nginx file for gastown.nhaboc.com found (got: ${src_conf:-none})."
  echo "        Add gtb.nhaboc.com -> 127.0.0.1:30881 next to the gastown entry, and 30881 to TERMINATOR_PORTS if nginx reaches NodePorts via the firewall."
fi

step "Town services (gt up)"
kubectl -n "$NS" exec gtb-0 -- sh -c 'cd /gt && gt up' || echo "gt up failed; run: kubectl -n $NS exec -it gtb-0 -- sh -c 'cd /gt && gt up'"

echo
echo "Deployed $IMAGE -> https://gtb.nhaboc.com (DNS: add a Cloudflare DNS-only A record gtb -> 100.76.86.103 if missing)"
