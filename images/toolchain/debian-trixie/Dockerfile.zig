# Toolchain image: Zig.
#
# Zig has no central package registry: `build.zig.zon` fetches dependencies by
# URL, so there is nothing to cache with a protocol adapter. The compiler is a
# self-contained tarball.
ARG BASE_IMAGE=toolchain-base:debian-trixie
FROM ${BASE_IMAGE}
COPY cache/zig-x86_64-linux-0.16.0.tar.xz /tmp/zig.tar.xz
RUN mkdir -p /opt/zig && tar -xJf /tmp/zig.tar.xz -C /opt/zig --strip-components=1 && rm /tmp/zig.tar.xz
ENV PATH=/opt/zig:$PATH
RUN zig version
