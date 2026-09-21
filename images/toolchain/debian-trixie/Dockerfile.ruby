# Toolchain image: Ruby + RubyGems.
#
# Generic: the tarball is pre-downloaded (fetch-artifacts.sh) and COPYed.
ARG BASE_IMAGE=toolchain-base:debian-trixie
FROM ${BASE_IMAGE}
COPY cache/ruby-4.0.7-ubuntu-22.04-x64.tar.gz /tmp/ruby.tar.gz
RUN mkdir -p /opt/ruby && tar -xzf /tmp/ruby.tar.gz -C /opt/ruby && rm /tmp/ruby.tar.gz
ENV PATH=/opt/ruby/bin:$PATH \
    LD_LIBRARY_PATH=/opt/ruby/lib
