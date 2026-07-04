# Per-Message Mail Processing Trace

Review status: implemented as the reusable `internal/mailtrace` runtime package
plus the authenticated `GET /admin/mail/messages/{messageId}/trace` private API.
The assembler expects the mail application tables named below and intentionally
uses only exact `model_invocation.message_id` correlation for prompt payloads.

Flowbee's own SQLite store does not own the Russell mail tables, so the runtime
trace endpoint reads an explicitly configured mail trace database instead of
`Store.DB`. Set `mail_trace_database_url` / `FLOWBEE_MAIL_TRACE_DATABASE_URL`
for `flowbee serve`; Postgres URLs use pgx and Postgres placeholders. The package
keeps the production Postgres DDL as `mailtrace.PostgresMigrationSQL` and tests
the runtime against fixture tables.

Mail LLM write paths should call the stage-specific helpers
`mailtrace.CreateLightComprehensionInvocation` and
`mailtrace.CreateHeavyComprehensionInvocation`, or their `WithDialect` variants,
inside the same transaction that creates invocation metadata. Those helpers force
the exact `message_id` + stage correlation required by the trace API.

## Purpose

The trace view lets an internal operator open one email message and inspect the
exact processing cascade that produced its ranking and comprehension outputs:

1. deterministic Stage 1 facts and the Stage 2 routing decision,
2. the light LLM invocation, prompt, raw response, and parsed verdict,
3. the heavy LLM escalation decision, context manifest, prompt, raw response,
   and parsed output,
4. every model invocation exactly correlated to the message.

Trace reads must never infer invocation relationships from prompt text,
timestamps, sender, subject, or other fuzzy signals.

## Required Correlation

Prefer adding nullable message correlation directly to `model_invocation`:

```sql
ALTER TABLE model_invocation
  ADD COLUMN message_id uuid NULL REFERENCES email_message(id);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_model_invocation_message_id
  ON model_invocation(message_id);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_model_invocation_message_stage_created
  ON model_invocation(message_id, stage, created_at);
```

Use the actual primary-key type of `email_message.id`. If
`model_invocation` does not have a stage or purpose discriminator, add
`stage text NULL` and write values such as:

- `mail_comprehension_light`
- `mail_comprehension_heavy`
- `mail_rank_debug`
- future mail-specific stages for draft generation, push verdicts, and other
  user-visible mail decisions

If direct modification of `model_invocation` is too risky for the migration
path, use an explicit correlation table instead:

```sql
CREATE TABLE mail_processing_trace_invocation (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  message_id uuid NOT NULL REFERENCES email_message(id),
  model_invocation_id uuid NOT NULL REFERENCES model_invocation(id),
  stage text NOT NULL,
  request_id text NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (model_invocation_id),
  UNIQUE (message_id, stage, model_invocation_id)
);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_trace_invocation_message_stage
  ON mail_processing_trace_invocation(message_id, stage, created_at);
```

The API and UI must hide which storage strategy is used.

## Write Path

Every mail-processing LLM call must persist exact correlation when invocation
metadata is created:

- light comprehension: `message_id` plus stage `mail_comprehension_light`,
- heavy comprehension: `message_id` plus stage `mail_comprehension_heavy`,
- any future mail-specific model call that contributes to ranking,
  comprehension, escalation, draft generation, push verdicts, or other
  user-visible mail decisions.

Persist the correlation in the same transaction as the invocation metadata when
possible. Payload rows may be written later; a trace can show correlated
metadata while marking prompt/response as pending.

Existing uncorrelated rows must remain uncorrelated unless an exact existing
request ID or foreign key can safely map them to a message. Do not backfill from
prompt contents, subject, sender, or timestamps.

## API

Expose a superadmin/internal endpoint:

```http
GET /admin/mail/messages/:messageId/trace
```

Only superadmin or internal users may access it. Prompt and response payloads
must not be exposed through normal user APIs in the first slice.

The response is message-centric and stable:

```json
{
  "message_id": "uuid",
  "message": {
    "subject": "string",
    "from": "string",
    "to": ["string"],
    "cc": ["string"],
    "received_at": "timestamp"
  },
  "deterministic": {
    "status": "complete",
    "stage1_band": "needs_llm",
    "stage2_prompt_key": "light_personal_open_loop",
    "routing_decision": {
      "selected_prompt_key": "light_personal_open_loop",
      "why": [
        "stage1_band=needs_llm",
        "known_contact=true",
        "recipient_position=to"
      ]
    },
    "facts": {
      "known_contact": {
        "value": true,
        "source": "mail_item_score.details.known_contact",
        "raw": null
      },
      "contact_type": {
        "value": "personal",
        "source": "mail_item_score.details.contact_type",
        "raw": null
      },
      "vip": {
        "value": false,
        "source": "mail_item_score.details.vip",
        "raw": null
      },
      "recipient_position": {
        "value": "to",
        "source": "mail_item_score.details.recipient_position",
        "raw": null
      },
      "first_party": {
        "value": true,
        "source": "mail_item_score.details.first_party",
        "raw": null
      },
      "sender_lean": {
        "value": "reply_worthy",
        "source": "mail_item_score.details.sender_lean",
        "raw": null
      },
      "correlations": {
        "value": [],
        "source": "mail_item_score.details.correlations",
        "raw": null
      },
      "entity_match": {
        "value": null,
        "source": "mail_item_score.details.entity_match",
        "raw": null
      }
    },
    "rank_rationale": "string",
    "raw_details": {}
  },
  "light_llm": {
    "status": "complete",
    "skip_reason": null,
    "invocation": {
      "id": "uuid",
      "request_id": "string",
      "model": "string",
      "model_version": "string",
      "prompt_version": "string",
      "started_at": "timestamp",
      "completed_at": "timestamp",
      "latency_ms": 1234,
      "cost": { "amount": 0.0012, "currency": "USD" },
      "status": "succeeded",
      "provider": "string",
      "error": null
    },
    "request_text": "verbatim prompt and injected context",
    "response_text": "verbatim raw response",
    "parsed_verdict": {
      "content_class": "string",
      "scores": {},
      "summary": "string",
      "key_points": [],
      "quick_reply": null,
      "open_loop": null,
      "escalate": false,
      "escalate_reason": null
    }
  },
  "heavy_llm": {
    "status": "skipped",
    "skip_reason": "light_llm_did_not_escalate",
    "escalated": false,
    "escalation_reason": null,
    "context_bundle_manifest": [],
    "invocation": null,
    "request_text": null,
    "response_text": null,
    "parsed_output": {
      "draft": null,
      "options": [],
      "belief_delta": null,
      "push_verdict": null
    }
  },
  "invocations": []
}
```

## Trace Assembler

Implement a `MailMessageTraceService` in the mail application rather than
building complex joins in the controller. The service should issue bounded
queries for:

- `email_message`,
- `mail_item_score`,
- `email_message_comprehension`,
- `email_message_comprehension_heavy`,
- correlated `model_invocation` rows,
- `model_invocation_payload` for those invocation IDs.

Select primary light and heavy invocations by exact message correlation plus
stage. If multiple invocations exist for a stage, use the latest successful row.
If none succeeded, use the latest row and surface its status or error. List all
correlated rows in the raw invocation section.

## Deterministic Section

For messages with `mail_item_score`, include:

- `stage1_band`,
- `stage2_prompt_key`,
- `rank_rationale`,
- raw `details` JSON,
- normalized Stage 1 facts for known contact, contact type, VIP, recipient
  position, first-party, `sender_lean`, correlations, entity match, and any
  other Stage 1 fact already present in `details`,
- source paths inside `mail_item_score.details` where practical,
- raw values whenever normalization changes representation or reads nested
  keys.

The routing explanation must be deterministic. Use explicit recorded routing
reasons if present; otherwise derive a concise explanation from `stage1_band`,
`stage2_prompt_key`, and the normalized facts used by the router.

## Light LLM Section

Show:

- exact prompt text from `model_invocation_payload.request_text`,
- exact raw response text from `model_invocation_payload.response_text`,
- invocation metadata including ID, request ID, provider, model, model version,
  status, timestamps, latency, cost, and error fields where available,
- `email_message_comprehension.prompt_version`,
- parsed verdict fields: `content_class`, scores, summary, key points,
  quick reply, open loop, `escalate`, and escalation reason.

If parsed comprehension exists but no exact invocation correlation exists, keep
the parsed verdict and mark request/response as
`unavailable_legacy_uncorrelated`.

## Heavy LLM Section

Show:

- whether the message escalated,
- why it escalated or did not escalate,
- context bundle manifest,
- exact heavy prompt and raw response from the correlated payload row,
- invocation metadata equivalent to the light section,
- parsed heavy output including draft, options, belief delta, push verdict, and
  any other structured fields already stored.

If the heavy table does not persist a context bundle manifest, add:

```sql
ALTER TABLE email_message_comprehension_heavy
  ADD COLUMN context_bundle_manifest jsonb NULL;
```

The manifest describes included context items: item type, source ID,
title/label, token estimate where available, and inclusion reason.

## Status Semantics

Use these stage statuses consistently:

- `complete`: required data for the stage exists and no failure is recorded,
- `pending`: upstream or current processing has not completed and may still run,
- `skipped`: stage intentionally did not run; include `skip_reason`,
- `missing`: expected data is absent with no known pending job or skip reason,
- `failed`: invocation or processing record indicates failure; include error
  details where available.

Examples:

- no `mail_item_score`: deterministic `pending` for newly ingested active work,
  otherwise `missing`,
- `stage2_prompt_key=null`: light LLM `skipped` with
  `deterministic_route_no_llm`,
- light verdict `escalate=false`: heavy LLM `skipped` with
  `light_llm_did_not_escalate`,
- heavy invocation exists but parsed heavy row is absent: heavy LLM `pending` or
  `failed` based on invocation status,
- parsed legacy comprehension with no correlated invocation: parsed section is
  present and prompt payload is `unavailable_legacy_uncorrelated`.

## Superadmin UI

Add a superadmin trace page at the app's existing internal route convention,
preferably:

```text
/admin/mail/messages/:messageId/trace
```

Flowbee also exposes a minimal internal message list at:

```text
/admin/mail/messages
```

Each row links to the message-centric trace page so operators can click from the
internal mail surface into the full cascade. The trace page should render:

1. message header,
2. deterministic Stage 1,
3. routing decision,
4. light LLM,
5. heavy LLM,
6. raw invocations.

Prompt and response blocks must preserve whitespace and be copyable. Missing,
skipped, failed, and pending stages must be visually distinct and include the
reason or error.

## Security

The endpoint is internal-only. Do not log prompt or response payloads from trace
handlers. Apply existing secret redaction for API keys or credentials before
rendering payloads; do not redact ordinary email content in this internal view
unless the app's existing policy requires it.

The future product mail drawer can consume the same backend concepts but should
hide raw prompt and response payloads by default.

## Test Coverage

The mail application should cover:

- complete deterministic plus light plus heavy trace,
- deterministic route that skips LLM,
- light completion that does not escalate,
- heavy invocation pending parsed output,
- parsed legacy comprehension with no exact invocation,
- failed invocation with error metadata,
- API authorization and response shape,
- write-path persistence of message correlation for light and heavy calls,
- UI rendering of complete and partial traces, including collapsible prompt and
  response blocks.
