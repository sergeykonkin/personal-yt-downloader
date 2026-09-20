---
name: download-youtube-video-shortcut
description: Maintain the Download YouTube video iOS Shortcut plist in this repository. Use when editing Shortcut behavior, input handling, API request wiring, import questions, metadata, or building a signed .shortcut via `make shortcut`. Do not use for backend-only Go changes.
---

# Download YouTube video Shortcut

Maintain the Shortcut as repository-owned source without depending on an
external Shortcut-authoring plugin.

## Source and outputs

- Treat `shortcuts/Download-YouTube-Video.plist` as the only source of truth.
- Treat `shortcuts/build/` and every `*.shortcut` file as generated artifacts.
  They are ignored by Git.
- Keep the display name in `WFWorkflowName` as `Download YouTube video`; it is
  independent of the plist filename.
- Read [references/plist-wiring.md](references/plist-wiring.md) before
  changing actions, variable references, control flow, or import questions.

## Editing workflow

1. Inspect the plist and identify the affected actions by
   `WFWorkflowActionIdentifier`, UUID, and current array index.
2. Make a targeted edit. Preserve existing UUIDs and `GroupingIdentifier`
   values for unchanged actions and control-flow groups.
3. Generate uppercase UUIDs with `uuidgen` for new output-producing actions
   or new control-flow groups.
4. Recalculate every `WFWorkflowImportQuestions[].ActionIndex` from the final
   action array. Confirm that each question points to an action containing
   its `ParameterKey`.
5. Update the authoritative documentation for the changed concern. Keep
   build commands in the Makefile (`make shortcut`); update the root
   `README.md` for user-visible behavior and `AGENTS.md` for repository
   architecture or invariants.
6. Run the checks below before reporting completion.

Do not add generator branding or provenance comments to the Shortcut.
Comments inside the workflow describe current behavior and wiring only.

## Security

- Never populate the plist with the deployed origin, a real Bearer token, or
  any other secret; the source is committed.
- Keep the API token import question's `DefaultValue` empty.
- Never place the deployed origin, Bearer token, request body, video title,
  or downloader output in documentation or logs.
- Preserve the Authorization header on the job submission action.

## Validation

Run from the repository root:

```sh
plutil -lint shortcuts/Download-YouTube-Video.plist
git diff --check
```

Also inspect the final plist as JSON and verify these semantic invariants:

- all control-flow start actions have matching end actions with the same
  `GroupingIdentifier`;
- every `OutputUUID` resolves to an existing action UUID;
- every `WFTextTokenString` placeholder has a matching `attachmentsByRange`
  entry at the correct character position;
- the submit body reads the named `Video URL` variable;
- the two import questions target the API origin and token text parameters;
- the token question has an empty default;
- the workflow accepts URL and string Share Sheet inputs.

## Building and signing

When the user requests a build, installation, or testable file, run
`make shortcut` (macOS only — it needs Apple's `shortcuts` CLI). It lints the
plist, signs it, and writes `shortcuts/build/Download YouTube video.shortcut`. Verify
that the signed output exists and is nonempty, and do not commit it.
