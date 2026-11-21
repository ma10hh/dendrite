# Runs Complement itself in a container, for hosts whose firewall does not let containers connect to the host.
# The source of Complement is mounted at /complement, and so is the Docker socket (see run.sh).
FROM golang:1.26-bookworm
WORKDIR /complement
ENV COMPLEMENT_IN_DOCKER_NETWORK=1 \
    GOFLAGS=-buildvcs=false
