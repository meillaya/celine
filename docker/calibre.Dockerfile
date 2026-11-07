# syntax=docker/dockerfile:1
#
# Headless Calibre Content Server image.
#
# The upstream Linux installer is pinned to an explicit release and checks the
# download's SHA-512 signature itself. The image only carries the server, not
# the desktop application, and keeps all mutable state under /config.

FROM debian:bookworm-slim

ARG CALIBRE_VERSION=9.18.0

ENV DEBIAN_FRONTEND=noninteractive \
    CALIBRE_CONFIG_DIRECTORY=/config \
    HOME=/config \
    LANG=C.UTF-8

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        python3 \
        xz-utils \
        libegl1 \
        libgl1 \
        libglx-mesa0 \
        libopengl0 \
        libxcb-cursor0 \
        libfreetype6 \
        libfontconfig1 \
        libx11-6 \
        libx11-xcb1 \
        libxcb1 \
        libxcb-icccm4 \
        libxcb-image0 \
        libxcb-keysyms1 \
        libxcb-randr0 \
        libxcb-render-util0 \
        libxcb-shape0 \
        libxcb-sync1 \
        libxcb-xfixes0 \
        libxcb-xkb1 \
        libxkbcommon0 \
        libglib2.0-0 \
        libdbus-1-3 \
        libxi6 \
        libxtst6 \
        libxrender1 \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL -o /tmp/linux-installer.sh https://download.calibre-ebook.com/linux-installer.sh \
    && sh /tmp/linux-installer.sh install_dir=/opt isolated=true version="${CALIBRE_VERSION}" \
    && rm -f /tmp/linux-installer.sh \
    && mkdir -p /config \
    && HOME=/tmp /opt/calibre/calibre-server --version

COPY calibre-entrypoint.sh /usr/local/bin/calibre-entrypoint.sh
RUN chmod 0755 /usr/local/bin/calibre-entrypoint.sh

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/ >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/calibre-entrypoint.sh"]
