package mailurgency

import (
	"context"
	"database/sql"
	"fmt"
)

const cleanupLegacyAttentionUrgencySQL = `
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
`

const cleanupLegacyNeedUrgencySQL = `
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
`

func CleanupLegacyUrgency(ctx context.Context, db *sql.DB) (int64, error) {
	required := []string{"mail_messages", "mail_comprehensions", "tenant_domains"}
	for _, table := range required {
		ok, err := tableExists(ctx, db, table)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, nil
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin mail urgency cleanup: %w", err)
	}
	var total int64
	if ok, err := tableExistsTx(ctx, tx, "mail_attention_items"); err != nil {
		_ = tx.Rollback()
		return 0, err
	} else if ok {
		res, err := tx.ExecContext(ctx, cleanupLegacyAttentionUrgencySQL)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("apply mail attention urgency cleanup: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if ok, err := tableExistsTx(ctx, tx, "mail_need_items"); err != nil {
		_ = tx.Rollback()
		return 0, err
	} else if ok {
		res, err := tx.ExecContext(ctx, cleanupLegacyNeedUrgencySQL)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("apply mail need urgency cleanup: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit mail urgency cleanup: %w", err)
	}
	return total, nil
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
		return false, fmt.Errorf("check table %s: %w", table, err)
	}
	return n > 0, nil
}

func tableExistsTx(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
		return false, fmt.Errorf("check table %s: %w", table, err)
	}
	return n > 0, nil
}
