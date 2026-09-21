# Toolchain image: Scala.
#
# scala-cli is the modern launcher (it can run scripts, manage a project, and
# drive Bloop); sbt remains the incumbent build tool for large or legacy
# projects. Both are shipped so either workflow works. Extends the java
# toolchain for the JDK, Gradle and the cacerts trust the egress CA lands in.
ARG BASE_IMAGE=toolchain-java:debian-trixie
FROM ${BASE_IMAGE}
COPY cache/scala-cli-x86_64-pc-linux.gz /tmp/scala-cli.gz
RUN gunzip -c /tmp/scala-cli.gz > /usr/local/bin/scala-cli && chmod +x /usr/local/bin/scala-cli && rm /tmp/scala-cli.gz
COPY cache/sbt-1.13.0.tgz /tmp/sbt.tgz
RUN tar -xzf /tmp/sbt.tgz -C /opt && rm /tmp/sbt.tgz
ENV PATH=/opt/sbt/bin:$PATH
RUN scala-cli version && sbt --script-version
