# Download YouTube video Shortcut

`Download-YouTube-Video.plist` is the source of truth for the iOS Shortcut. It is
an XML property list containing the complete workflow definition. The display
name is stored in `WFWorkflowName`; the source filename does not control the
name shown by Shortcuts.

The Shortcut accepts a YouTube URL from the iOS Share Sheet or prompts for a
URL when opened directly. It POSTs the URL to `/api/jobs` with the Bearer
token and finishes — no polling, no waiting, no downloading. The job runs on
the server; the shortcut only confirms it was queued.

## Build

Signing requires macOS with Apple's `shortcuts` CLI. From the repository
root:

```sh
make shortcut
```

The target lints the plist, signs it, and writes
`shortcuts/build/Download YouTube video.shortcut`. Open that file on an Apple device
to import it. The two import questions configure the site's HTTPS origin and
the Bearer token (`API_TOKEN`).

`*.shortcut` files and `shortcuts/build/` are generated artifacts ignored by
Git. Commit the plist source, not a signed build.
