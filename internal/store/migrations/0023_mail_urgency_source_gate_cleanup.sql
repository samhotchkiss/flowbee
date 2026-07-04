-- 0023: neutralize legacy regex-only mail urgency created before the
-- comprehension source gates. Flowbee's test/control-plane database does not
-- normally own mail tables, so the compatibility DDL keeps this migration safe
-- in repos where the mail subsystem is absent while still running the cleanup
-- when legacy rows are present.

CREATE TABLE IF NOT EXISTS mail_attention_items (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    source TEXT NOT NULL,
    priority TEXT NULL,
    impact_statement TEXT NULL,
    llm_verdict_id TEXT NULL,
    classification_source TEXT NULL,
    user_visible INTEGER NOT NULL DEFAULT 1,
    invalidated_reason TEXT NULL
);

CREATE TABLE IF NOT EXISTS mail_need_items (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    source TEXT NOT NULL,
    priority TEXT NULL,
    impact_statement TEXT NULL,
    llm_verdict_id TEXT NULL,
    derivation_source TEXT NULL,
    user_visible INTEGER NOT NULL DEFAULT 1,
    invalidated_reason TEXT NULL
);

CREATE TABLE IF NOT EXISTS mail_messages (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT '',
    sender_email TEXT NOT NULL DEFAULT '',
    sender_domain TEXT NOT NULL DEFAULT '',
    headers TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS mail_comprehensions (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT '',
    verdict_recorded INTEGER NOT NULL DEFAULT 0,
    personal_ask INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS tenant_domains (
    tenant_id TEXT NOT NULL,
    domain TEXT NOT NULL
);

WITH candidate AS (
    SELECT
        a.id,
        CASE
            WHEN lower(trim(COALESCE(m.status, ''))) IN ('sent', 'draft', 'drafted') THEN 'sent_or_draft'
            WHEN EXISTS (
                SELECT 1
                  FROM tenant_domains td
                 WHERE td.tenant_id = a.tenant_id
                   AND (
                        lower(rtrim(COALESCE(NULLIF(m.sender_domain, ''), substr(m.sender_email, instr(m.sender_email, '@') + 1)), '.')) = lower(rtrim(td.domain, '.'))
                        OR lower(rtrim(COALESCE(NULLIF(m.sender_domain, ''), substr(m.sender_email, instr(m.sender_email, '@') + 1)), '.')) LIKE '%.' || lower(rtrim(td.domain, '.'))
                   )
            ) THEN 'first_party_sender'
            WHEN lower(COALESCE(m.headers, '')) LIKE '%list-unsubscribe%'
             AND NOT EXISTS (
                SELECT 1
                  FROM mail_comprehensions c
                 WHERE c.tenant_id = a.tenant_id
                   AND c.message_id = a.message_id
                   AND lower(trim(c.status)) = 'completed'
                   AND COALESCE(c.verdict_recorded, 0) = 1
                   AND COALESCE(c.personal_ask, 0) = 1
            ) THEN 'bulk_without_personal_ask'
            WHEN NOT EXISTS (
                SELECT 1
                  FROM mail_comprehensions c
                 WHERE c.tenant_id = a.tenant_id
                   AND c.message_id = a.message_id
                   AND lower(trim(c.status)) = 'completed'
                   AND COALESCE(c.verdict_recorded, 0) = 1
            ) THEN 'missing_llm_verdict'
            ELSE ''
        END AS reason
    FROM mail_attention_items a
    LEFT JOIN mail_messages m
      ON m.tenant_id = a.tenant_id
     AND m.id = a.message_id
    WHERE lower(trim(COALESCE(a.source, ''))) IN ('mail', 'email')
      AND (
        lower(trim(COALESCE(a.classification_source, ''))) IN ('regex', 'regex_v1', 'legacy_regex', 'rule', 'rules')
        OR COALESCE(a.llm_verdict_id, '') = ''
      )
      AND (
        lower(trim(COALESCE(a.priority, ''))) IN ('p0', 'p1', 'urgent', 'critical')
        OR COALESCE(a.impact_statement, '') <> ''
      )
      AND COALESCE(a.user_visible, 1) = 1
),
invalid AS (
    SELECT id, reason FROM candidate WHERE reason <> ''
)
UPDATE mail_attention_items
   SET priority = CASE
        WHEN lower(trim(COALESCE(priority, ''))) IN ('p0', 'p1', 'urgent', 'critical') THEN NULL
        ELSE priority
    END,
    impact_statement = NULL,
    user_visible = 0,
    invalidated_reason = (SELECT reason FROM invalid WHERE invalid.id = mail_attention_items.id)
 WHERE id IN (SELECT id FROM invalid);

WITH candidate AS (
    SELECT
        n.id,
        CASE
            WHEN lower(trim(COALESCE(m.status, ''))) IN ('sent', 'draft', 'drafted') THEN 'sent_or_draft'
            WHEN EXISTS (
                SELECT 1
                  FROM tenant_domains td
                 WHERE td.tenant_id = n.tenant_id
                   AND (
                        lower(rtrim(COALESCE(NULLIF(m.sender_domain, ''), substr(m.sender_email, instr(m.sender_email, '@') + 1)), '.')) = lower(rtrim(td.domain, '.'))
                        OR lower(rtrim(COALESCE(NULLIF(m.sender_domain, ''), substr(m.sender_email, instr(m.sender_email, '@') + 1)), '.')) LIKE '%.' || lower(rtrim(td.domain, '.'))
                   )
            ) THEN 'first_party_sender'
            WHEN lower(COALESCE(m.headers, '')) LIKE '%list-unsubscribe%'
             AND NOT EXISTS (
                SELECT 1
                  FROM mail_comprehensions c
                 WHERE c.tenant_id = n.tenant_id
                   AND c.message_id = n.message_id
                   AND lower(trim(c.status)) = 'completed'
                   AND COALESCE(c.verdict_recorded, 0) = 1
                   AND COALESCE(c.personal_ask, 0) = 1
            ) THEN 'bulk_without_personal_ask'
            WHEN NOT EXISTS (
                SELECT 1
                  FROM mail_comprehensions c
                 WHERE c.tenant_id = n.tenant_id
                   AND c.message_id = n.message_id
                   AND lower(trim(c.status)) = 'completed'
                   AND COALESCE(c.verdict_recorded, 0) = 1
            ) THEN 'missing_llm_verdict'
            ELSE ''
        END AS reason
    FROM mail_need_items n
    LEFT JOIN mail_messages m
      ON m.tenant_id = n.tenant_id
     AND m.id = n.message_id
    WHERE lower(trim(COALESCE(n.source, ''))) IN ('mail', 'email')
      AND (
        lower(trim(COALESCE(n.derivation_source, ''))) IN ('regex', 'regex_v1', 'legacy_regex', 'rule', 'rules', 'message.received')
        OR COALESCE(n.llm_verdict_id, '') = ''
      )
      AND (
        lower(trim(COALESCE(n.priority, ''))) IN ('p0', 'p1', 'urgent', 'critical')
        OR COALESCE(n.impact_statement, '') <> ''
      )
      AND COALESCE(n.user_visible, 1) = 1
),
invalid AS (
    SELECT id, reason FROM candidate WHERE reason <> ''
)
UPDATE mail_need_items
   SET priority = CASE
        WHEN lower(trim(COALESCE(priority, ''))) IN ('p0', 'p1', 'urgent', 'critical') THEN NULL
        ELSE priority
    END,
    impact_statement = NULL,
    user_visible = 0,
    invalidated_reason = (SELECT reason FROM invalid WHERE invalid.id = mail_need_items.id)
 WHERE id IN (SELECT id FROM invalid);
