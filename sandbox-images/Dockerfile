# syntax=docker/dockerfile:1
# Sandbox image: an existing GENERIC toolchain image + the agent-worker binary
# baked in. This is the ONLY kind of image a sandbox may run now (the gateway
# no longer injects the worker at launch).
#
# BASE_IMAGE is one of the deployment's stage-1 toolchain images, e.g.
#   <registry>/agent-toolchain/toolchain-base:debian-trixie
#   <registry>/agent-toolchain/toolchain-node:debian-trixie
# The worker binary is cross-compiled from the gateway's vendored worker-src/.
#
# No egress CA is baked: this deployment reaches the internet through an HTTP
# proxy, not a MITM sidecar, so the toolchain image's own trust store is enough.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

# agent-worker as PID 1: it brings its own shell (mvdan/sh), so no shell is
# needed at runtime. WORKER_PORT matches the port the gateway expects (48080).
COPY agent-worker /usr/local/bin/agent-worker
RUN mkdir -p /workspace /data
ENV WORKER_PORT=48080 \
    WORKER_WORKSPACE=/workspace \
    WORKER_DB=/data/jobs.db
EXPOSE 48080
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/agent-worker"]
