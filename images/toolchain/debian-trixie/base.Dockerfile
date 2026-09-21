# Stage 1: the GENERIC toolchain base.
#
# Plain Debian trixie slim plus the utilities a dev workflow expects, INCLUDING
# build-essential (gcc/g++/make) so any language runtime can compile native code
# on demand (node-gyp, pip sdists, native gems, cargo `cc` crates, Erlang NIFs).
# Every language toolchain image is built on this base.
#
# Nothing here knows about the workspace deployment: no worker binary, no CA.
# (`clang` is the one exception: it is built standalone WITHOUT gcc by design.)
#
#   build-toolchain.sh base   # ./Dockerfile -> ${TOOLCHAIN_REPO}-base
ARG BASE_IMAGE=debian:trixie-slim
FROM ${BASE_IMAGE}

# The build runs through the mihomo proxy (buildkitd env). deb.debian.org over
# the proxy is ~500x faster than the in-region aliyun mirror (benchmarked:
# 13.5 MB/s vs 25 KB/s), so the OFFICIAL host is used directly — no mirror swap.
ARG APT_MIRROR=deb.debian.org

ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8 LC_ALL=C.UTF-8

# Base utilities every dev workflow expects. The official sources are already
# what the image ships, so nothing is restored afterwards.
RUN set -eux; \
    . /etc/os-release; \
    case "${VERSION_CODENAME:-trixie}" in \
      trixie) SUITES="trixie trixie-updates"; SEC="trixie-security" ;; \
      *)      SUITES="bookworm bookworm-updates"; SEC="bookworm-security" ;; \
    esac; \
    printf 'Types: deb\nURIs: http://%s/debian\nSuites: %s\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\nTypes: deb\nURIs: http://%s/debian-security\nSuites: %s\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n' \
        "$APT_MIRROR" "$SUITES" "$APT_MIRROR" "$SEC" > /etc/apt/sources.list.d/debian.sources; \
    apt-get -o Acquire::Retries=10 update; \
    for i in 1 2 3 4 5 6; do \
      apt-get -o Acquire::Retries=10 install -y --no-install-recommends \
        ca-certificates curl wget git openssh-client xz-utils unzip zip bzip2 zstd \
        build-essential pkg-config file procps jq less gnupg \
      && break || { echo "apt retry $i"; sleep 3; }; \
    done; \
    rm -rf /var/lib/apt/lists/*; \
    printf 'Types: deb\nURIs: http://deb.debian.org/debian\nSuites: %s\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\nTypes: deb\nURIs: http://deb.debian.org/debian-security\nSuites: %s\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n' \
        "$SUITES" "$SEC" > /etc/apt/sources.list.d/debian.sources
