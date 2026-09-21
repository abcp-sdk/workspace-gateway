# Toolchain image: Swift 6.4 (Debian build).
#
# Generic: the toolchain tarball is pre-downloaded (fetch-artifacts.sh) and
# COPYed. The runtime/compile dependencies come from apt (base image sources).
ARG BASE_IMAGE=toolchain-base:debian-trixie
FROM ${BASE_IMAGE}
COPY cache/swift-6.4.0-RELEASE-debian13.tar.gz /tmp/swift.tar.gz
RUN set -eux; \
    apt-get -o Acquire::Retries=10 update; \
    apt-get -o Acquire::Retries=10 install -y --no-install-recommends \
      libc6-dev binutils libcurl4t64 libedit2 libncurses6 libsqlite3-0 libxml2 libz3-4 tzdata zlib1g-dev libpython3-dev libstdc++-14-dev; \
    rm -rf /var/lib/apt/lists/*; \
    mkdir -p /opt/swift && tar -xzf /tmp/swift.tar.gz -C /opt/swift --strip-components=1 && rm /tmp/swift.tar.gz
ENV PATH=/opt/swift/usr/bin:$PATH
