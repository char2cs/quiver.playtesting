# Quiver Playtesting

<p align="center">
  <img src="https://raw.githubusercontent.com/rabbytesoftware/quiver.core/develop/.github/quiver.svg" alt="Quiver" width="450" />
  <br/>
  <em>Let people try your game from a link. Nothing else gets exposed.</em>
</p>

## What it does

Quiver Playtesting is a small gateway that puts a test machine in a browser tab. You give a playtester a link. They open it and see a noVNC desktop of one virtual machine, and run their session there. They install nothing and never learn where the machine is.

Each link is tied to one VM, expires on its own, and allows one live session at a time. Close the tab and the link can be used again until it expires.

## What it does not do

- It does not expose VNC. The VNC ports stay on your LAN. The gateway connects to them itself and signs in with the VNC password, which playtesters never see.
- It does not offer clipboard sharing or file transfer, only the screen, keyboard and mouse.
- It has no web admin. You manage everything from a terminal on the server, so there is no login page to attack.

## How it works

1. You run the arrow. It listens on one port and talks plain HTTP, so put a proxy such as Cloudflare in front for TLS and open only that port on your network.
2. In the admin TUI you add a VM (host, port, VNC password) and create a link for a playtester. The TUI shows the link once.
3. The playtester opens `https://<your host>/s/<token>`. The gateway checks the token, connects to the VM and relays the screen over a WebSocket.
4. You can watch live sessions, flag one that is acting up, or drop it, which closes the connection immediately and can revoke the link.

The gateway only believes the client IP header from your proxy's addresses (Cloudflare's by default), so nobody can dodge a ban by faking it. It refuses to connect to link-local and metadata addresses, caps a session at four hours, and shuts down cleanly on stop. Tokens are random and stored only as hashes. A bad token, an expired link and a busy VM all look the same from outside, and repeated bad guesses get an address banned for a while.

## Running the TUI

The gateway runs as the arrow's service. To manage it, open a shell on the same machine:

```
cd <install path>
./quiver-playtest tui --data ./data --public-url https://<your host>
```

Your data (VMs, links, session history) lives in the `data` folder and survives updates and uninstalls.

```arrow
schema: "arrow@v0"

metadata:
  name: char2cs.quiver-playtesting
  description: Playtesting gateway that serves one VM per link to the browser over noVNC, without exposing VNC ports
  version: 0.1.0
  license: MIT
  url: https://github.com/char2cs/quiver.playtesting
  quiver: github.com/char2cs/quiver.playtesting
  maintainers:
    - name: char2cs
      url: https://char2cs.net
  media:
    icon: "https://raw.githubusercontent.com/rabbytesoftware/quiver.core/develop/docs/quiver-icon.svg"
    banner: "https://raw.githubusercontent.com/rabbytesoftware/quiver.core/develop/docs/quiver-banner.svg"
  tags:
    - playtesting
    - vnc
    - novnc
    - gateway

variables:
  - name: PUBLIC_HOST
    type: string
    default: playtesting.quiver.ar
    description: Public hostname playtesters use, checked against the Origin header
  - name: REAL_IP_HEADER
    type: string
    default: CF-Connecting-IP
    description: Header carrying the real client IP behind the proxy, empty to use the socket peer
  - name: TRUSTED_PROXIES
    type: string
    default: cloudflare
    description: Proxies whose real-IP header is believed, as CIDRs or the word cloudflare. Use none to ignore the header
  - name: MAX_SESSION
    type: string
    default: 4h
    description: Hard cap on one session's length, as a Go duration
  - name: MAX_SESSIONS
    type: number
    default: "50"
    description: Maximum concurrent playtesting sessions
    min: 1
    max: 1000
  - name: IDLE_TIMEOUT
    type: string
    default: 15m
    description: End a session with no traffic for this long, as a Go duration

netbridge:
  - name: GATEWAY_PORT
    default: 8480
    protocol: tcp
    required: true

targets:
  "linux/*":
    requirements:
      cpu_cores: 1
      ram_gb: 1
      disk_gb: 1

    lifecycle:
      install:
        - type: fetch
          url:
            default: https://github.com/char2cs/quiver.playtesting/releases/download/nightly/quiver-playtest_nightly_linux_amd64.tar.gz
            linux/arm64: https://github.com/char2cs/quiver.playtesting/releases/download/nightly/quiver-playtest_nightly_linux_arm64.tar.gz
          to: ./quiver-playtest.tar.gz
          title: Downloading quiver-playtest
          timeout: 5m

        - type: run
          command: tar -xzf ./quiver-playtest.tar.gz quiver-playtest
          title: Extracting quiver-playtest
          timeout: 1m

        - type: run
          command: chmod 0755 ./quiver-playtest
          title: Setting executable bit
          timeout: 10s

        - type: run
          command: rm -f ./quiver-playtest.tar.gz
          title: Cleaning up
          timeout: 10s
          exit_on_failure: false

      update:
        - type: fetch
          url:
            default: https://github.com/char2cs/quiver.playtesting/releases/download/nightly/quiver-playtest_nightly_linux_amd64.tar.gz
            linux/arm64: https://github.com/char2cs/quiver.playtesting/releases/download/nightly/quiver-playtest_nightly_linux_arm64.tar.gz
          to: ./quiver-playtest.tar.gz
          title: Downloading quiver-playtest
          timeout: 5m

        - type: run
          command: tar -xzf ./quiver-playtest.tar.gz quiver-playtest
          title: Replacing quiver-playtest
          timeout: 1m

        - type: run
          command: rm -f ./quiver-playtest.tar.gz
          title: Cleaning up
          timeout: 10s
          exit_on_failure: false

      execute:
        - type: run
          command: ./quiver-playtest serve --listen 0.0.0.0:${GATEWAY_PORT} --public-host ${PUBLIC_HOST} --real-ip-header ${REAL_IP_HEADER} --trusted-proxies ${TRUSTED_PROXIES} --max-session ${MAX_SESSION} --max-conns ${MAX_SESSIONS} --idle-timeout ${IDLE_TIMEOUT} --data ${INSTALL_PATH}/data
          title: Starting playtesting gateway
          timeout: 30s

      stop:
        - type: signal
          signal: graceful
          timeout: 15s
          exit_on_failure: false

      uninstall:
        - type: run
          command: rm -f ./quiver-playtest ./quiver-playtest.tar.gz
          title: Removing binary (data directory is kept)
          timeout: 30s
          exit_on_failure: false
```
