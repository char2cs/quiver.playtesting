# Test VM

A throwaway desktop (Xvfb + openbox + xterm + pcmanfm + mousepad) exposing VNC
on port 5900, for exercising the playtesting gateway locally.

    docker compose up -d --build
    nc 127.0.0.1 5900 | head -c 12     # RFB 003.008

Register it in quiver-playtest with host `127.0.0.1`, port `5900`, password
`secret12` (VNC auth uses at most 8 characters). The port is published on
127.0.0.1 only. Change the password with `VNC_PASSWORD`, the size with
`RESOLUTION`.

    docker compose down
