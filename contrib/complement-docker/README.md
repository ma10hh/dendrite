# Complement with working federation, in a container

[Complement](https://github.com/matrix-org/complement) runs the homeserver in a container and plays the other homeservers of the federation tests itself, in the test process on the host. The homeserver reaches that process as `host.docker.internal`. On a host whose firewall drops traffic from the Docker bridges to the host, every one of those tests times out: **about a third of the tests of `./tests` cannot pass**.

This directory runs Complement itself in a container on the network of the homeservers, so that nothing has to reach the host:

```bash
contrib/complement-docker/run.sh                    # ./tests and ./tests/csapi, with the dendrite_blacklist tag
TAGS= contrib/complement-docker/run.sh              # without the tag (runs the tests that Dendrite skips)
PACKAGES=./tests RUN='TestKnocking' SKIP_BUILD=1 IMAGE=complement-dendrite:local contrib/complement-docker/run.sh
```

It builds the Dendrite image from the checkout, clones Complement at the revision the patch is for into `~/.cache/dendrite-complement`, applies `complement-in-docker.patch`, and runs `go test -json` in a `golang` container with the Docker socket mounted. The result is `./complement-run.json`, and the script prints the numbers of passing, failing and skipped tests.

## What the patch changes in Complement

It is switched by `COMPLEMENT_IN_DOCKER_NETWORK=1`, which the container sets. Without it Complement works as before.

- Complement connects its own container to the network of each deployment, with `host.docker.internal` (the value of `COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT`) as its alias, so that the homeservers resolve that name to it by Docker's DNS.
- It does not add the `host-gateway` entry for that name to the homeservers any more.
- It reaches the homeservers by their address on that network, `http://<ip>:8008` and `https://<ip>:8448`, not through published ports on `127.0.0.1`, which would be its own container.

## Requirements and limits

- Docker (the socket is mounted into the container; the user needs the right to use it) and network access to pull `golang:1.26-bookworm` and to clone Complement.
- The patch is for the Complement revision in `run.sh`. For a newer one, update it: `git diff` in a patched clone.
- Containers of the deployments are named `complement_*` and removed by Complement at the end of a run; a run that is killed leaves them behind (`docker ps -a --filter name=complement_`).
- Run it on a machine that nothing else uses: a full run takes 30 to 40 minutes, and the tests are timing sensitive.
