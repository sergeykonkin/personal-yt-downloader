# Shortcut plist wiring

Read this reference when changing the action array, variables, control flow,
HTTP action, or import questions in `shortcuts/Download-YouTube-Video.plist`.

## Input selection

The workflow supports two entry points that converge on the named variable
`Video URL`:

1. `If` checks Shortcut Input with condition `100` (`has any value`).
2. The first branch stores `ExtensionInput` in `Video URL`.
3. `Otherwise` asks for a URL with `is.workflow.actions.ask` and
   `WFInputType = URL`.
4. The manual branch stores the Ask for Input action's `Provided Input`
   output in `Video URL`.
5. `End If` closes the group.

`ExtensionInput` is a magic variable attachment. Do not add
`is.workflow.actions.input` as a runtime action.

The job-submission JSON value for `url` is a `WFTextTokenString` containing
one object-replacement placeholder (`U+FFFC`). Its attachment is a
named-variable reference:

```xml
<dict>
  <key>Type</key><string>Variable</string>
  <key>VariableName</key><string>Video URL</string>
</dict>
```

## Configuration and import questions

The API origin and Bearer token are Text actions so import questions can
replace `WFTextActionText`. The source contains a harmless placeholder
origin and token marker, while the token question's `DefaultValue` is an
empty string.

Each import question targets a stable action UUID and one of that action's
parameters:

| Question | Target action UUID | Action identifier | Parameter |
| --- | --- | --- | --- |
| API origin | `B545E7F0-6612-4B5B-B04B-142C26B3991F` | `is.workflow.actions.gettext` | `WFTextActionText` |
| Bearer token | `40D94962-75C7-428B-B7A9-D9B36149A863` | `is.workflow.actions.gettext` | `WFTextActionText` |

`ActionIndex` is zero-based and refers to the final `WFWorkflowActions`
array. Derive it by locating the target UUID in that final array; never copy
a prior numeric index across an insert, removal, or reorder. Confirm that the
located action still has the expected identifier and parameter. If a target
action is intentionally replaced and receives a new UUID, update this table.

## API flow

The Shortcut performs one request:

1. `POST /api/jobs` with JSON `{ "url": Video URL }` and the
   `Authorization: Bearer <token>` header.
2. Show the notification and finish. The job runs on the server; the
   Shortcut never polls or downloads.

The API answers `202 Accepted` (or `200` with `duplicate: true` when the video
is already queued) — both are 2xx, so the action succeeds. Any failure status
(401 bad token, 429 queue full or rate-limited, 400 invalid URL, …) makes the
Get Contents of URL action fail, and iOS shows its own error sheet; the
Shortcut has no error-handling actions by design.

The configured API origin supplies the URL prefix. Do not construct URLs from
response Host or forwarded headers.

## Variable serialization

- Use `WFTextTokenAttachment` for parameters whose entire value is one
  variable.
- Use `WFTextTokenString` when a variable is embedded in text, a URL, or a
  header.
- Each embedded variable is represented by `U+FFFC`; each
  `attachmentsByRange` key uses its exact zero-based character position and
  length `1`.
- Action outputs use `Type = ActionOutput`, `OutputUUID`, and the action's
  exported `OutputName`.
- Named variables use `Type = Variable` and `VariableName`.

## Control flow

Conditional groups use one shared `GroupingIdentifier` per group:

- `WFControlFlowMode = 0` starts the group;
- `WFControlFlowMode = 1` is `Otherwise` or a middle branch;
- `WFControlFlowMode = 2` ends the group.

Existence conditions use integer `WFCondition` values: `100` means
`has any value`, and `101` means `does not have any value`. They do not take
a comparison literal.

## Top-level metadata

- `WFWorkflowTypes` contains `ActionExtension` for Share Sheet availability.
- `WFWorkflowInputContentItemClasses` contains both
  `WFURLContentItem` and `WFStringContentItem`.
- `WFWorkflowIcon` contains both the glyph and color values.
- `WFWorkflowName` remains `Download YouTube video`.
