# Email Policy Engine

Review status: reviewed product/engineering spec for v1 implementation.

This spec defines the policy layer for Russell's email comprehension pipeline. The
layer runs after classification and before surfacing or similar-item stacking. Its
job is to decide, per classified message, whether Russell should archive silently,
archive and report an aggregate counter, extract structured information, draft a
reply for review, or surface the message.

The v1 goal is narrow: stop flat surfacing of safe, repetitive low-value classes
such as promotions, newsletters, receipts, and duplicate spam-moderation pings,
while preserving visibility for personal, financial-risk, security, sensitive,
unknown, low-confidence, and reply-worthy mail.

## Placement

The email pipeline must run in this order:

1. Fetch candidate messages.
2. Run comprehension and classification.
3. Evaluate policy from message metadata, `content_class`, `importance_band`,
   classifier confidence, sender history, and tenant policy memory.
4. Execute allowed mailbox actions through the existing `email.triage` MCP tool.
5. Write one `processed_ledger` policy entry for every decided message.
6. Emit aggregate Brief counters for `act-and-report` groups.
7. Pass only surfaced survivors to the similar-item stacker.
8. Render stacked and surfaced items in the Brief.

Policy owns consume/archive/extract/draft/surface decisions. The stacker only
groups messages that survived policy handling, and it must not override a policy
decision.

## Decision Contract

Each decision input must include at least:

- `tenant_id`
- `message_id`
- `thread_id`, when available
- `sender_email`
- `sender_domain`
- `subject`
- `received_at`
- `content_class`
- `importance_band`
- classifier confidence, when available
- matched tenant sender override, when available
- recent sender/class history, when available
- duplicate hints, when already available outside the stacker

Each decision output must be structured and versioned:

```json
{
  "policy_version": "email-policy-v1",
  "decision": "archive_and_report",
  "action": "archive",
  "reporting": "aggregate_counter",
  "extraction": null,
  "drafting": null,
  "reason_code": "class_default_promotion_low",
  "matched_rule": "class:promotion:any_band",
  "safety_gate": null
}
```

Required fields are `policy_version`, `decision`, `action`, `reporting`,
`reason_code`, and `matched_rule`. Optional fields are `extraction`, `drafting`,
`safety_gate`, `override_id`, and `notes`.

## Canonical Decisions

| Decision | Action | Ledger disposition | Brief behavior |
| --- | --- | --- | --- |
| `archive_silently` | Archive | `act-silently` | Hidden unless diagnostics are enabled |
| `archive_and_report` | Archive | `act-and-report` | Aggregate counter line |
| `extract_archive_and_report` | Extract, then archive | `act-and-report` | Aggregate extraction/counter line |
| `extract_and_surface` | Extract only | Surface item | Individual or stacked item |
| `surface` | None | Surface item | Individual or stacked item |
| `draft_and_surface` | Draft only, never send | Surface draft for review | Individual item |
| `hold_for_gate` | None | Surface or diagnostic hold | Individual item with reason |

V1 mailbox mutations are limited to archive/unarchive through `email.triage`.
No policy default may unsubscribe, delete, mark spam, click links, fill forms,
send replies, or perform non-mailbox external writes.

## V1 Class Defaults

Unknown or ambiguous classes fail open to `surface`.

| `content_class` | Band handling | Default decision | Brief wording |
| --- | --- | --- | --- |
| `promotion`, `promo` | Any safe band | `archive_and_report` | `N promotions archived` |
| `newsletter` | Low/normal | `archive_and_report` | `N newsletters archived` |
| `receipt` | Low/normal | `archive_and_report` unless a reliable extractor is already wired | `N receipts archived` |
| `wordpress_spam_moderation` and equivalent moderation pings | Low/normal repetitive messages | `archive_and_report` for the aggregate group; silent per item after it is counted | `N WordPress spam-moderation pings archived` |
| `statement` | Low/normal | `extract_archive_and_report` | `N statements extracted and archived` |
| `renewal` | Low/normal without risk terms | `extract_archive_and_report` | `N renewals extracted and archived` |
| `shipping_update` | Low routine shipped/delivered mail | `archive_and_report` | `N shipping updates archived` |
| `calendar_invite` | Any | `surface` | Individual or stacked |
| `personal` | Any | `surface` | Individual or stacked |
| `money_problem`, payment failure, fraud, collections | Any | `surface` | Individual or stacked, priority follows band |
| `security_alert` | Any | `surface` | Individual or stacked, priority follows band |
| `legal`, `tax`, `medical`, `employment`, `school`, `government` | Any | `surface` | Individual or stacked |
| `reply_request` and reply-worthy classes | Any | `hold_for_gate` until reply gates pass | Individual item |

Safety overrides:

- Any `high`, `urgent`, `critical`, or equivalent band surfaces unless the class
  is explicitly safe and an exact sender archive override applies.
- Any low-confidence classifier result surfaces.
- Extraction failure in an archive-after-extract class surfaces with the failure
  reason.
- User-facing deadlines, cancellation risk, payment failure, refund issues,
  account lock, fraud, legal notices, or required responses surface regardless
  of base class.

## Sender Policy Memory

Policy memory is tenant-scoped. V1 must support exact sender archive/surface
overrides and should use a schema that can also represent domain and sender/class
overrides:

```sql
email_policy_overrides (
  id UUID PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  scope_type TEXT NOT NULL,
  scope_value TEXT NOT NULL,
  content_class TEXT NULL,
  disposition TEXT NOT NULL,
  reporting TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  disabled_at TIMESTAMPTZ NULL,
  reason TEXT NULL
)
```

Normalize sender emails and domains to lowercase. Enforce one active override per
`(tenant_id, scope_type, scope_value, content_class)`.

Precedence:

1. `always_surface_sender`
2. `class_override_for_sender`
3. `always_archive_sender`
4. `always_surface_domain`
5. `always_archive_domain`
6. Class default
7. Unknown fallback to `surface`

Surface overrides always win. Archive overrides must not suppress sensitive or
high-consequence classes unless the override is exact sender-level and the message
is low/normal band. Domain archive overrides must not archive `money_problem`,
`security_alert`, `legal`, `tax`, `medical`, `employment`, `school`, `government`,
or reply-worthy classes.

The v1 editable surface can be backend CRUD, an internal/admin endpoint, or an
MCP-accessible command. It must support listing active overrides, creating an
override, disabling an override, updating disposition/reporting, and auditing who
or what created the override. V1 may suggest sender overrides from repeated
archival history, but it must not create persistent overrides automatically.

## Ledger Requirements

Every policy decision writes to `processed_ledger`. Auto-handled rows must include:

- `tenant_id`
- `message_id`
- `thread_id`, when available
- `policy_version`
- `content_class`
- `importance_band`
- `disposition`
- `mailbox_action`
- `reporting_mode`
- `reason_code`
- `matched_rule`
- `override_id`, when applicable
- `triage_action_id` or equivalent execution correlation id, when available
- `processed_at`
- `brief_batch_id` or run id
- extraction artifact id, when extraction occurred
- execution status: `planned`, `succeeded`, `failed`, or `skipped_already_done`
- error details for failures

Idempotency rules:

- A message with an existing successful policy ledger entry for the same policy
  version must not be re-archived or re-reported.
- If `email.triage` reports that a message is already archived, record
  `skipped_already_done` or equivalent success and count it at most once.
- Failed executions remain retryable and visible in diagnostics; they are not
  counted as successful archive/extract actions.

## Brief Aggregation

`act-silently` entries are hidden by default. `act-and-report` entries produce one
aggregate Brief line per group:

```text
brief_batch_id + policy_version + content_class + disposition + reporting_mode + reason_code
```

Required examples for the `swh` corpus, assuming low/normal bands and no safety
exceptions:

- `42 promotions archived`
- `20 newsletters archived`
- `22 receipts archived`
- `16 WordPress spam-moderation pings archived`
- `6 statements extracted and archived`
- `3 renewals extracted and archived`

Aggregate metadata should retain count, user-readable class label, sender/domain
summary for homogeneous groups, time range, hidden message ids, extraction status
when present, and a reversible action hint such as unarchive through
`email.triage`. Failed or unhandled items are excluded from success counters and
surfaced or reported as separate diagnostics.

## Similar-Item Stacking

Policy runs before stacker candidate selection:

1. Apply policy to all classified messages.
2. Execute/archive/extract/write ledger for consumed messages.
3. Remove consumed messages from the stacker candidate set.
4. Pass only survivors to the stacker.
5. Render stacks in the Brief.

Promotions, newsletters, receipts, and moderation pings archived by policy must
not appear as stacks. Personal and high-risk survivors remain eligible for
stacking if the stacker and product UX allow it.

## Extraction and Reply Gates

Statements and renewals may archive after extraction only when an existing
extractor succeeds and no safety exception is detected. Extraction failures,
price increases, cancellation deadlines, failed payments, and required responses
surface.

Reply-worthy classes remain held or surfaced until both gates are complete:

- #3704 Sent/Archive folder sync
- #3705 Thread stitching

After those gates, reply policy may produce `draft_and_surface`. It must never
auto-send.

## Safety Rails

V1 policy must obey these hard limits:

- Never auto-unsubscribe.
- Never click links, fill forms, mark external systems, send replies, or perform
  non-mailbox external writes.
- Archive is the only zero-touch mailbox mutation, and it must remain reversible
  through `email.triage` unarchive.
- Never permanently delete messages.
- Never mark spam automatically.
- Never auto-archive sensitive/high-consequence classes listed in this spec.
- Never auto-archive unknown classes or low-confidence classifications.
- Surface extraction failures for archive-after-extract classes.
- Keep complete ledger reason codes, matched rules, execution status, and
  correlation ids for auditability.
- Support dry-run/shadow mode before tenant rollout.

## Observability and Rollout

Metrics/logs must count decisions by class/disposition, archived messages by
class, surfaced safety overrides, matched sender overrides, extraction failures,
triage failures, idempotent skips, and shadow-mode disagreements. Logs may carry
correlation ids and existing-safe metadata, but not raw email bodies.

Rollout:

1. Implement shadow mode that computes and ledgers policy without mailbox
   mutation.
2. Run against the `swh` corpus and verify expected aggregate counts:
   42 promotions, 20 newsletters, 22 receipts, and 16 WordPress moderation pings.
3. Verify personal, money-problem, security, sensitive, unknown, high-band, and
   low-confidence messages surface.
4. Enable archive execution for one internal/test tenant.
5. Compare Brief output before and after.
6. Enable tenant `swh` after review.
7. Add or expose sender override management.

## Buildable Issues

1. **Add versioned email policy decision engine.** Create a backend module that
   accepts classified message metadata and returns structured v1 decisions.
   Cover class defaults, unknown fallback, low confidence, sensitive classes, and
   high/urgent bands in unit tests.
2. **Add tenant-scoped sender policy overrides.** Persist auditable overrides,
   support exact sender always-archive and always-surface in v1, include domain
   and sender/class schema support if low-cost, and expose internal/admin CRUD.
3. **Wire policy decisions to `email.triage`.** Execute archive dispositions
   through the existing MCP tool path, handle already-archived as idempotent
   success, and support dry-run/shadow mode.
4. **Write policy results to `processed_ledger`.** Add policy fields, execution
   status, correlation ids, extraction artifact ids, and idempotency checks so
   reruns do not duplicate actions or Brief counters.
5. **Add Brief aggregate counter lines.** Aggregate successful `act-and-report`
   ledger entries, render concise counters, hide silent entries by default, and
   preserve drill-down metadata.
6. **Integrate policy with similar-item stacking.** Ensure policy runs before
   stacker candidate selection and tests prove archived items do not stack while
   surfaced survivors still can.
7. **Add extraction-aware archive-after-extract classes.** Run existing
   extraction for statements and renewals when available, archive only on success,
   ledger artifact ids, and surface failures or risk variants.
8. **Add safety and regression tests.** Fail tests if v1 policy defaults introduce
   unsubscribe, delete, spam marking, reply send, or non-mailbox external writes;
   cover sensitive class surfacing, reply gates, corpus counts, and survivor flow.

## Open Questions

- What are the canonical classifier enum names for all v1 classes and aliases?
- Does `processed_ledger` need a migration, or can policy fields live in existing
  metadata?
- Should Brief aggregation live in the email processing service or renderer?
- Is there an existing tenant settings/admin API suitable for override CRUD?
- Which extraction artifacts already exist for receipts, statements, and
  renewals, and what confidence threshold blocks archive-after-extract?
