# Session Recording: Design

## Goal

Every playtest session is recorded to an mp4 on the gateway host, from the moment the playtester connects to the moment they disconnect. Each link gets its own folder. Recording must never affect the live stream or the session itself.

## Non-goals

- No consent flow beyond the header notice (no popup, no acceptance step).
- No audio, no input overlay (keystrokes, cursor trail), no live viewing of the recording.
- No playback, download or deletion UI. The operator reads the files from disk.
- No per-link on/off switch: when recording is enabled, every session is recorded.
- No upload to remote storage.

## Decisions

- **Separate capture connection.** For each session the gateway opens a second VNC connection to the same VM, used only for recording. It asks for Raw and CopyRect encodings, which are trivial to decode, and the LAN to the VMs has bandwidth to spare. The live relay (`internal/rfb/relay.go`) is not touched. Both VMs allow shared connections (x11vnc runs with `-shared`, TightVNC is shared by default).
- **ffmpeg encodes.** Decoded frames are piped as raw video to an `ffmpeg` child process that writes H.264 mp4. ffmpeg is a runtime requirement on the gateway host. No usable pure-Go H.264 encoder exists.
- **Failure isolation.** A recorder error ends the recording only. The session continues and the error is logged. Missing ffmpeg disables recording at startup with a warning.

## Architecture

New package `internal/recorder`, with three parts:

- `capture.go`: the VNC client. `rfb.Dial` (existing handshake and VNC auth), `SetPixelFormat` to 32-bit true colour, `SetEncodings` [Raw, CopyRect, DesktopSize], a loop of incremental `FramebufferUpdateRequest` messages paced by the replies, and a decoder that applies rectangles to an in-memory RGBA framebuffer. Unknown or unexpected server messages end the capture (bounded sizes everywhere, same strictness as the relay parser).
- `encode.go`: starts `ffmpeg`, reads the framebuffer at a fixed rate (default 10 fps) and writes raw BGRA frames to its stdin. Output is H.264 (`libx264`, `-preset ultrafast`, `-crf 30`, `yuv420p`) in fragmented mp4 (`-movflags +frag_keyframe+empty_moov+default_base_moof`), so a file is playable even if the gateway or the host dies mid-session. The ffmpeg child runs at the lowest CPU priority.
- `recorder.go`: lifecycle and paths. `Start(ctx, SessionInfo)` returns a stop function. It creates the folder, runs capture and encode, and stops when the session context ends or the capture fails.

Gateway hook: in `serveWS`, after the VM dial and the browser handshake succeed, `h.rec.Start(ctx, sess, vm)` is called and its stop function deferred. `gateway.Config` gets a `Recorder` field (an interface with one method), nil when recording is off, so the gateway has no import of the recorder package.

### Resolution changes

The mp4 has a fixed size. If the VM reports a new desktop size (DesktopSize), the current file is finalised and a new segment starts (`..._part2.mp4`).

### Storage layout

```
<recordings-dir>/                       default <data>/recordings, mode 0700
  link-<id>-<label-slug>/               one folder per link, mode 0700
    20261004T153012Z_<session-id>.mp4   one file per session, mode 0600
```

A link can connect more than once (reconnects), so a folder can hold several files. The slug is lowercase `[a-z0-9-]`, at most 40 characters, so the label can never form a path. The `link-<id>` prefix keeps folders unique and stable.

### Resource limits

The gateway host is small (1 vCPU, about 15 GB free disk), so:

- `--recordings-max` (default 2) caps concurrent recordings. Extra sessions run unrecorded and the log says why.
- Recording stops when free space under the recordings dir drops below `--recordings-min-free` (default 2 GiB). The session continues.
- `--recordings-retention` (default 30 days, 0 keeps everything) deletes older files and empty folders, using the same prune loop pattern as the session log.

### Notice to playtesters

The page header shows a short notice, "This session is recorded", with a small red dot, next to the "Playtesting" label. It is plain text in the existing header style (`web/static/index.html`, `style.css`), with no popup and no click needed.

The notice must be true, so the gateway only shows it while recording is actually available. `index.html` contains a placeholder comment where the notice goes, and `serveIndex` fills it in when recording is enabled and ffmpeg was found at startup. When recording is off the header is unchanged. It still appears when the concurrent cap is reached, because the page cannot know that in advance.

### Visibility

- `core.Session` gets `Recording string`, the path of the first mp4 file (empty when not recording). `session_log` gets a `recording TEXT NOT NULL DEFAULT ''` column (added by a migration on startup) holding the same path.
- The TUI Sessions tab shows a REC marker for sessions being recorded.
- This lets the operator find the file for a logged session.

## Configuration

Flags or env, matching existing conventions:

| Flag | Env | Default |
| --- | --- | --- |
| `--recordings` | `QP_RECORDINGS` | `true` |
| `--recordings-dir` | `QP_RECORDINGS_DIR` | `<data>/recordings` |
| `--ffmpeg` | `QP_FFMPEG` | `ffmpeg` (looked up in PATH) |
| `--record-fps` | `QP_RECORD_FPS` | `10` |
| `--recordings-max` | `QP_RECORDINGS_MAX` | `2` |
| `--recordings-min-free` | `QP_RECORDINGS_MIN_FREE` | `2GiB` |
| `--recordings-retention` | `QP_RECORDINGS_RETENTION` | `720h` |

At startup, if recordings are on and ffmpeg is not found or not executable, the gateway logs a warning and runs with recording off. It does not refuse to start.

## Security

- Folders 0700 and files 0600. Names come only from numeric IDs, a fixed slug alphabet, a timestamp and the random session ID. No user-controlled path segment.
- The capture connection reuses `rfb.Dial`, so it keeps the existing SSRF guards and only connects to the VM registered in the store.
- ffmpeg is started with an explicit argument list (no shell), a fixed output path, and a closed environment apart from `PATH`.
- Recordings contain whatever the playtester does on the VM. The recordings dir is for the operator only and is never served over HTTP.

## Testing

- Framebuffer decoder: Raw and CopyRect rectangles, bounds checks, malformed input (fuzz target in line with `FuzzParser`).
- Capture against the existing fake VNC server (`internal/rfb/rfbtest`): connects, receives updates, handles DesktopSize and server disconnect.
- End to end: fake VNC server, real ffmpeg, session start and stop, then `ffprobe` checks that the mp4 exists, has the right size and a duration within tolerance. Skipped when ffmpeg is absent.
- Failure isolation: ffmpeg missing, ffmpeg crashing mid-session and a capture error each leave the session running.
- Limits: concurrent cap, low disk space, retention pruning, path building (slug and traversal cases).
- Header notice: `serveIndex` includes the notice when recording is enabled and omits it when it is off.
- Gateway hook: with a fake `Recorder`, start and stop are called once per session, including when the session ends by kill, revoke, idle and max duration.

## Deployment

- Install ffmpeg on the gateway host (`apt install ffmpeg` on Debian). The README and ARROW.md state the requirement.
- No change to the Cloudflare or network setup.
