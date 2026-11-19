#!/usr/bin/env bash
# Brings the lab up from scratch, seeds a user through MAS and runs test.sh.
# SLOW=1 also waits for an access token to expire (5 minutes). KEEP=1 leaves the lab running.
set -eu
cd "$(dirname "$0")"
./setup.sh
docker compose down -v --remove-orphans >/dev/null 2>&1
docker compose up -d >/dev/null 2>&1
for i in $(seq 1 90); do
  curl -sf --resolve dendrite:8008:127.0.0.1 http://dendrite:8008/_matrix/client/versions >/dev/null 2>&1 \
    && curl -sf --resolve mas:8080:127.0.0.1 http://mas:8080/.well-known/openid-configuration >/dev/null 2>&1 && break
  sleep 1
done
docker compose run --rm --no-deps -T mas manage register-user --config /config.yaml --yes \
  --password 'correct-horse-battery' --email alice@example.com alice >/dev/null 2>&1
sleep 3
STATUS=0
bash ./test.sh || STATUS=$?
docker compose logs dendrite 2>&1 | grep -E 'level=(error|fatal|panic)' > dendrite-errors.log || true
echo "Dendrite log lines at level error or worse: $(wc -l < dendrite-errors.log) (dendrite-errors.log)"
[ "${KEEP:-0}" = 1 ] || docker compose down -v --remove-orphans >/dev/null 2>&1
exit $STATUS
