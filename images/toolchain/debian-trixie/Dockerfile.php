# Toolchain image: PHP + Composer.
#
# Generic: the artifacts are pre-downloaded (fetch-artifacts.sh) and COPYed.
ARG BASE_IMAGE=toolchain-base:debian-trixie
FROM ${BASE_IMAGE}
COPY cache/php-8.5.8-cli-linux-x86_64.tar.gz /opt/php.tar.gz
COPY cache/composer.phar /opt/composer.phar
RUN mkdir -p /opt/php && tar -xzf /opt/php.tar.gz -C /opt/php --strip-components=1 && rm /opt/php.tar.gz \
 && printf '#!/bin/sh\nexec /opt/php/bin/php /opt/composer.phar "$@"\n' > /usr/local/bin/composer \
 && chmod +x /usr/local/bin/composer
ENV PATH=/opt/php/bin:$PATH
