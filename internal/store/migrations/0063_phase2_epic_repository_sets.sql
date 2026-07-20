-- 0063: explicit project-owned repository sets for epics.
-- epics.repo remains the compatibility projection of the single delivery repo.

ALTER TABLE epics ADD COLUMN repository_set_mode TEXT NOT NULL DEFAULT 'legacy'
    CHECK (repository_set_mode IN ('legacy','explicit'));
ALTER TABLE epics ADD COLUMN repository_set_finalized INTEGER NOT NULL DEFAULT 1
    CHECK (repository_set_finalized IN (0,1));

CREATE TABLE epic_repositories (
    epic_id                TEXT NOT NULL,
    project_id             TEXT NOT NULL,
    repo_id                TEXT NOT NULL,
    is_delivery            INTEGER NOT NULL DEFAULT 0 CHECK (is_delivery IN (0,1)),
    membership_validated   INTEGER NOT NULL DEFAULT 1 CHECK (membership_validated IN (0,1)),
    created_at             TEXT NOT NULL,
    PRIMARY KEY (epic_id,repo_id),
    FOREIGN KEY (epic_id) REFERENCES epics(id) ON DELETE CASCADE,
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE UNIQUE INDEX idx_epic_repositories_one_delivery
    ON epic_repositories(epic_id) WHERE is_delivery=1;
CREATE INDEX idx_epic_repositories_project_repo
    ON epic_repositories(project_id,repo_id,epic_id);

-- Every historical epic receives exactly its current compatibility repo as its
-- delivery repository. Default-project unregistered/empty repos are retained as
-- explicit legacy compatibility facts, never misrepresented as validated.
INSERT INTO epic_repositories
    (epic_id,project_id,repo_id,is_delivery,membership_validated,created_at)
SELECT e.id,e.project_id,e.repo,1,
       CASE WHEN EXISTS (
         SELECT 1 FROM project_repos pr JOIN projects p ON p.id=pr.project_id
          JOIN repos r ON r.id=pr.repo_id
          WHERE pr.project_id=e.project_id AND pr.repo_id=e.repo
            AND pr.state='active' AND p.state='active' AND r.active=1
       ) THEN 1 ELSE 0 END,
       e.created_at
  FROM epics e;

CREATE TRIGGER epic_repository_owner_membership_insert
BEFORE INSERT ON epic_repositories
WHEN (EXISTS (SELECT 1 FROM epics e WHERE e.id=NEW.epic_id
               AND e.repository_set_finalized=1)
      AND EXISTS (SELECT 1 FROM epic_repositories er WHERE er.epic_id=NEW.epic_id))
 OR NOT EXISTS (SELECT 1 FROM epics e
                  WHERE e.id=NEW.epic_id AND e.project_id=NEW.project_id)
 OR (NEW.is_delivery=1 AND NOT EXISTS (
       SELECT 1 FROM epics e WHERE e.id=NEW.epic_id AND e.repo=NEW.repo_id))
 OR (NEW.membership_validated=1 AND NOT EXISTS (
       SELECT 1 FROM project_repos pr JOIN projects p ON p.id=pr.project_id
        JOIN repos r ON r.id=pr.repo_id
        WHERE pr.project_id=NEW.project_id AND pr.repo_id=NEW.repo_id
          AND pr.state='active' AND p.state='active' AND r.active=1))
 OR (NEW.membership_validated=0 AND NOT EXISTS (
       SELECT 1 FROM epics e WHERE e.id=NEW.epic_id AND e.project_id='default'
         AND e.repository_set_mode='legacy' AND NEW.is_delivery=1))
BEGIN
  SELECT RAISE(ABORT,'epic repository ownership or membership mismatch');
END;

CREATE TRIGGER epic_repository_finalize_valid
BEFORE UPDATE OF repository_set_finalized ON epics
WHEN OLD.repository_set_finalized=0 AND NEW.repository_set_finalized=1
 AND (NOT EXISTS (SELECT 1 FROM epic_repositories er WHERE er.epic_id=OLD.id)
      OR 1<>(SELECT COUNT(*) FROM epic_repositories er
             WHERE er.epic_id=OLD.id AND er.is_delivery=1))
BEGIN
  SELECT RAISE(ABORT,'epic repository set requires exactly one delivery repository');
END;

CREATE TRIGGER epic_repository_finalize_one_way
BEFORE UPDATE OF repository_set_finalized ON epics
WHEN OLD.repository_set_finalized=1 AND NEW.repository_set_finalized<>1
BEGIN
  SELECT RAISE(ABORT,'epic repository set cannot be reopened');
END;

CREATE TRIGGER epic_repository_identity_immutable
BEFORE UPDATE OF epic_id,project_id,repo_id,is_delivery,membership_validated ON epic_repositories
WHEN OLD.epic_id<>NEW.epic_id OR OLD.project_id<>NEW.project_id
  OR OLD.repo_id<>NEW.repo_id OR OLD.is_delivery<>NEW.is_delivery
  OR OLD.membership_validated<>NEW.membership_validated
BEGIN
  SELECT RAISE(ABORT,'epic repository set is immutable after admission');
END;

CREATE TRIGGER epic_repository_no_direct_delete
BEFORE DELETE ON epic_repositories
WHEN EXISTS (SELECT 1 FROM epics WHERE id=OLD.epic_id)
BEGIN
  SELECT RAISE(ABORT,'epic repository set is immutable after admission');
END;

CREATE TRIGGER epic_repository_projection_immutable
BEFORE UPDATE OF repo,project_id,repository_set_mode ON epics
WHEN OLD.repo<>NEW.repo OR OLD.project_id<>NEW.project_id
  OR OLD.repository_set_mode<>NEW.repository_set_mode
BEGIN
  SELECT RAISE(ABORT,'epic repository projection is immutable after admission');
END;

-- Direct/legacy inserts also get a complete repository-set projection. Native
-- Store admission validates explicit sets before this trigger executes.
CREATE TRIGGER epic_repository_delivery_after_epic_insert
AFTER INSERT ON epics
BEGIN
  INSERT INTO epic_repositories
      (epic_id,project_id,repo_id,is_delivery,membership_validated,created_at)
  VALUES (NEW.id,NEW.project_id,NEW.repo,1,
      CASE WHEN EXISTS (
        SELECT 1 FROM project_repos pr JOIN projects p ON p.id=pr.project_id
         JOIN repos r ON r.id=pr.repo_id
         WHERE pr.project_id=NEW.project_id AND pr.repo_id=NEW.repo
           AND pr.state='active' AND p.state='active' AND r.active=1
      ) THEN 1 ELSE 0 END,
      NEW.created_at);
END;
