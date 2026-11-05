# The linux-tools profile's image: Debian with the static-analysis
# tools the profile advertises (jadx, apktool, radare2, binwalk,
# Python), plus the JRE jadx/apktool need and unzip/curl. Tool
# downloads happen here, at build time; the environment runs with no
# network. jadx and apktool are pinned to the same versions as the
# apk-decompile worker image so decoded output matches across tools.
FROM debian:bookworm-slim AS radare2-build

ARG RADARE2_VERSION=6.2.4
ARG RADARE2_SHA256=d88a27bdc4da2a5faa4b4223e0e92e6d6d8906dd46b98ea91475d851d703986d

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl meson ninja-build pkg-config python3 xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && curl --fail --location \
        "https://github.com/radareorg/radare2/releases/download/${RADARE2_VERSION}/radare2-${RADARE2_VERSION}.tar.xz" \
        -o /tmp/radare2.tar.xz \
    && echo "${RADARE2_SHA256}  /tmp/radare2.tar.xz" | sha256sum --check --strict \
    && mkdir /tmp/radare2 \
    && tar --extract --xz --file /tmp/radare2.tar.xz --directory /tmp/radare2 --strip-components=1 \
    && cd /tmp/radare2 \
    && meson setup build --prefix=/usr/local --buildtype=release \
    && meson compile -C build \
    && meson install -C build \
    && rm -rf /tmp/radare2 /tmp/radare2.tar.xz

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        binwalk ca-certificates curl default-jre-headless python3 unzip \
    && rm -rf /var/lib/apt/lists/*

COPY --from=radare2-build /usr/local/ /usr/local/
RUN ldconfig

RUN curl --fail --location https://github.com/skylot/jadx/releases/download/v1.5.6/jadx-1.5.6.zip -o /tmp/jadx.zip \
    && mkdir /opt/jadx \
    && unzip -q /tmp/jadx.zip -d /opt/jadx \
    && rm /tmp/jadx.zip \
    && ln -s /opt/jadx/bin/jadx /usr/local/bin/jadx \
    && curl --fail --location https://github.com/iBotPeaches/Apktool/releases/download/v3.0.3/apktool_3.0.3.jar -o /opt/apktool.jar \
    && printf '#!/bin/sh\nexec java -jar /opt/apktool.jar "$@"\n' > /usr/local/bin/apktool \
    && chmod 0755 /usr/local/bin/apktool

ENV PATH="/opt/jadx/bin:${PATH}"
WORKDIR /work
CMD ["sleep", "infinity"]
