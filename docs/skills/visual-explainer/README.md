# visual-explainer (vendored)

This directory is a **verbatim vendored copy** of the `visual-explainer` skill —
`SKILL.md`, `templates/`, and `references/` — from
**github.com/nicobailon/visual-explainer** (`plugins/visual-explainer/`), plus the
project's MIT `LICENSE`. No rewrites; see upstream for history and updates.

## Why it lives here

Every epic maintains a human-facing `epics/<slug>-explainer.html` on its branch (see
`epics/INSTRUCTIONS.md` → "## Explainer"): a self-contained HTML page (mermaid + prose)
that tells a reviewer what the epic is building and where it stands. `SKILL.md` is the
method for producing those pages.

**Both Claude and Codex epic runners should follow `SKILL.md`** when authoring or
refreshing an epic's explainer. The explainer is for humans only — `## Status` remains
the machine-parsed source of truth; the explainer is never parsed by automation.

Origin: github.com/nicobailon/visual-explainer · License: MIT (see `LICENSE`).
