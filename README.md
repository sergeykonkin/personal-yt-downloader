# personal-yt-downloader

A small, single-user, mobile-first website for downloading YouTube videos.
Open it on your phone, paste a link, watch the progress, and grab a
fast-start H.264 MP4 — or stream it from a 24-hour secret link in VLC — then
swipe the card away when you're done with it.

## Quick start

Requirements: Docker — that's it for running the app. The dev image
bundles yt-dlp (with its yt-dlp-ejs script package), ffmpeg, and the deno
runtime that current yt-dlp needs for full YouTube extraction.

```sh
make run-dev   # build the dev image, serve on http://localhost:8080
```

Password is `local` (override by exporting `PASSWORD` before `make
run-dev`). Optionally export `API_TOKEN` as well to accept
`Authorization: Bearer <token>` for direct API requests (curl, iOS
Shortcuts) against the `/api/jobs*` endpoints — unset means off. The
container (Ctrl-C stops it) publishes port 8080, keeps downloads under
`./data`, and uses plain-HTTP dev cookies.

## iOS Shortcut

For one-tap submits from the YouTube app, an iOS Shortcut ("Download
YouTube video" in the Share Sheet) POSTs the shared URL to `/api/jobs` with
the `API_TOKEN` Bearer header and finishes — no polling or waiting; the
download runs on the server. The source is
[`shortcuts/Download-YouTube-Video.plist`](shortcuts/Download-YouTube-Video.plist);
on macOS, `make shortcut` lints and signs an importable build into
`shortcuts/build/`. Importing asks two setup questions: the site's HTTPS
origin and the `API_TOKEN` value.

Working on the code itself needs Go 1.27 and Node.js on the host:
`make test` runs the Go suite (race detector) plus vitest, and `make build`
compiles the server to `bin/server` (a direct host run of that binary
additionally needs `yt-dlp` + `ffmpeg` on PATH). The frontend dev server
(`cd web && npm run dev`) proxies `/api`, `/m`, and `/healthz` to the
backend on `127.0.0.1:8080`.

Everything technical — architecture, configuration, the media pipeline,
the security model, and testing — lives in [AGENTS.md](AGENTS.md).
