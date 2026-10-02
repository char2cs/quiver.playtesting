FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      xvfb x11vnc openbox tint2 xterm pcmanfm mousepad dbus-x11 \
      fonts-dejavu-core procps ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash tester
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

USER tester
ENV DISPLAY=:0 RESOLUTION=1280x800 VNC_PASSWORD=secret12
EXPOSE 5900
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
