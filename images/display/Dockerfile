# syntax=docker/dockerfile:1
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# The cella-display image of spec 014: the desktop the computer-use operations
# of spec 023 run against. It carries an X server, a window manager, the
# capture tool, the input tool and a browser, and nothing that decides who may
# use them.
#
# The build context is the repository root, so the supervisor this image bakes
# in is the same file runtime/display embeds and the podman driver writes into
# a sandbox:
#
#     podman build -f images/display/Dockerfile -t cella-display:dev .
#
# Two ways to use it. On k8s it is the operator's CELLA_K8S_DISPLAY_IMAGE and
# runs as the Pod's second container beside any workload image. On podman the
# desktop is a process in the sandbox's own container, so this is the image a
# desktop sandbox names in spec.image.
FROM debian:bookworm-slim

# BROWSER is the browser a browser-ready sandbox drives. Building with
# --build-arg BROWSER= leaves it out, which is a much smaller image for a
# desktop nobody browses from.
ARG BROWSER=chromium

# The desktop's parts, one line per role:
#   xvfb          the X server the desktop runs on
#   openbox       the window manager, which registers the EWMH property the
#                 readiness probe reads
#   x11-apps      xwd, the root-window capture
#   x11-utils     xdpyinfo and xprop, the two halves of the readiness probe
#   imagemagick   the encoder that turns a capture into a PNG or a JPEG
#   xdotool       the pointer and the keyboard
#   fonts-dejavu  text that renders as text rather than as boxes
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --no-install-recommends --yes \
        ca-certificates \
        fonts-dejavu-core \
        imagemagick \
        openbox \
        x11-apps \
        x11-utils \
        xdotool \
        xvfb \
        ${BROWSER} \
    && rm -rf /var/lib/apt/lists/*

# ImageMagick's Debian policy refuses to read an X window dump, which is
# exactly what the capture pipes into it. The desktop's own frame buffer is
# not a document from an untrusted source, so the one format the capture needs
# is allowed back.
RUN sed -i 's|<policy domain="coder" rights="none" pattern="XWD" />||' /etc/ImageMagick-6/policy.xml || true

# The supervisor is the one copy: runtime/display embeds this same file, so a
# desktop this image starts and one the podman driver writes are the same
# desktop.
COPY runtime/display/start.sh /usr/local/bin/cella-display-start
RUN chmod 0755 /usr/local/bin/cella-display-start

# The desktop runs as an unprivileged account with a read-only root file
# system, so everything it writes is under /tmp, which is the one writable
# mount the security baseline of spec 004 grants and, on k8s, the volume the
# workload shares.
RUN useradd --uid 1000 --create-home --shell /bin/sh cella
USER 1000:1000
ENV HOME=/tmp/cella-display \
    XDG_RUNTIME_DIR=/tmp/cella-display/run \
    DISPLAY=:0 \
    CELLA_DISPLAY_GEOMETRY=1280x800x24

CMD ["/usr/local/bin/cella-display-start"]
