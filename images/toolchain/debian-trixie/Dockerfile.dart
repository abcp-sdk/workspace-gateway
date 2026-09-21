# Toolchain image: Dart (pub).
#
# Generic: the SDK zip is pre-downloaded (fetch-artifacts.sh) and COPYed.
ARG BASE_IMAGE=toolchain-base:debian-trixie
FROM ${BASE_IMAGE}
COPY cache/dartsdk-linux-x64-release.zip /tmp/dart.zip
RUN unzip -q /tmp/dart.zip -d /opt && mv /opt/dart-sdk /opt/dart && rm /tmp/dart.zip
ENV PATH=/opt/dart/bin:$PATH
