#!/usr/bin/env bash
# Runs Complement against the checkout, with Complement in a container so that federation tests work on hosts
# where containers cannot connect to the host (see README.md).
#
#   contrib/complement-docker/run.sh                       # ./tests ./tests/csapi, with the dendrite_blacklist tag
#   TAGS= contrib/complement-docker/run.sh                 # without the tag
#   PACKAGES="./tests/csapi" RUN='TestThreadsEndpoint' contrib/complement-docker/run.sh
#
# Environment: IMAGE (default complement-dendrite:local), SKIP_BUILD=1 to use an image that exists,
# COMPLEMENT_DIR (where Complement is cloned, default ~/.cache/dendrite-complement), OUT (result file, default
# ./complement-run.json), PACKAGES, RUN, TAGS, TIMEOUT.
set -euo pipefail

# The Complement revision that complement-in-docker.patch applies to.
COMPLEMENT_REV=5051189644 # full ID: 50511896440c2f1ceb627c6f6f46c4a313ee0955

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
IMAGE="${IMAGE:-complement-dendrite:local}"
COMPLEMENT_DIR="${COMPLEMENT_DIR:-$HOME/.cache/dendrite-complement}"
OUT="${OUT:-$PWD/complement-run.json}"
PACKAGES="${PACKAGES:-./tests ./tests/csapi}"
TAGS="${TAGS-dendrite_blacklist}"
TIMEOUT="${TIMEOUT:-3h}"

if [ -z "${SKIP_BUILD:-}" ]; then
  DOCKER_BUILDKIT=1 docker build -t "$IMAGE" -f "$root/build/scripts/Complement.Dockerfile" --build-arg CGO=0 "$root"
fi

if [ ! -d "$COMPLEMENT_DIR/.git" ]; then
  git clone https://github.com/matrix-org/complement "$COMPLEMENT_DIR"
fi
(
  cd "$COMPLEMENT_DIR"
  git checkout --quiet --force "$COMPLEMENT_REV" 2>/dev/null || { git fetch --quiet origin && git checkout --quiet --force "$COMPLEMENT_REV"; }
  git reset --quiet --hard "$COMPLEMENT_REV"
  git apply "$here/complement-in-docker.patch"
)

docker build -q -t complement-runner:local "$here" >/dev/null
docker volume create complement-gomod >/dev/null

runargs=()
[ -n "${RUN:-}" ] && runargs+=(-run "$RUN")
tagargs=()
[ -n "$TAGS" ] && tagargs+=(-tags "$TAGS")

docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$COMPLEMENT_DIR":/complement \
  -v complement-gomod:/go/pkg/mod \
  -e COMPLEMENT_BASE_IMAGE="$IMAGE" \
  complement-runner:local \
  go test ${tagargs[@]+"${tagargs[@]}"} ${runargs[@]+"${runargs[@]}"} -json -count=1 -timeout "$TIMEOUT" $PACKAGES > "$OUT"

echo "Results in $OUT. Summary:"
jq -r 'select(.Test!=null and (.Action=="pass" or .Action=="fail" or .Action=="skip")) | .Action' "$OUT" | sort | uniq -c
