# A no-op build that re-serves an upstream image under the workspace registry.
# BuildKit pulls BASE_IMAGE and re-pushes it unchanged, so the upstream image
# config (ENTRYPOINT/CMD/ENV/USER/EXPOSE/WORKDIR/labels) is preserved verbatim.
#
# Only a runnable IMAGE manifest can be mirrored this way; arbitrary OCI
# artifacts (Helm charts, SBOMs) are not images and cannot be `FROM`-ed.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
