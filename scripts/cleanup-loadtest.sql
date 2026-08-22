-- Safe removal of load-test data (loadtest.py) from a production database.
--
-- It removes only users of the form lt_0000, lt_0001, … (see USER_PREFIX in
-- loadtest.py) and everything attached to them. Real accounts are untouched.
--
-- A regular expression rather than LIKE: in SQL `LIKE 'lt_%'` treats `_` as
-- "any single character" and would also match a real user such as "ltAlex".
-- The anchored `^lt_[0-9]+$` matches the lt_ prefix followed by digits only.
--
-- Usage:
--   sudo -u postgres psql -d messenger -f scripts/cleanup-loadtest.sql
-- Everything runs in one transaction, so an unexpected match can be aborted
-- (Ctrl-C before COMMIT rolls back). It prints what it will delete first.

\set ON_ERROR_STOP on
\pset pager off

BEGIN;

-- ── Preview: who and how many would be affected ────────────────────────────
\echo '=== Users to be deleted (first 5 and the total) ==='
SELECT username FROM users WHERE username ~ '^lt_[0-9]+$' ORDER BY username LIMIT 5;
SELECT count(*) AS lt_users        FROM users     WHERE username ~ '^lt_[0-9]+$';
SELECT count(*) AS lt_messages     FROM messages  WHERE sender ~ '^lt_[0-9]+$' OR recipient ~ '^lt_[0-9]+$';
SELECT count(*) AS lt_reactions    FROM reactions WHERE username ~ '^lt_[0-9]+$'
                                       OR msg_id IN (SELECT id FROM messages
                                                     WHERE sender ~ '^lt_[0-9]+$' OR recipient ~ '^lt_[0-9]+$');

-- ── Deletion in a foreign-key-safe order ───────────────────────────────────
-- 1) Reactions, both by these users and on the messages about to go.
DELETE FROM reactions
 WHERE username ~ '^lt_[0-9]+$'
    OR msg_id IN (SELECT id FROM messages
                  WHERE sender ~ '^lt_[0-9]+$' OR recipient ~ '^lt_[0-9]+$');

-- 2) Messages where such a user is the sender or the recipient.
DELETE FROM messages
 WHERE sender ~ '^lt_[0-9]+$' OR recipient ~ '^lt_[0-9]+$';

-- 3) Group membership (the load test creates none, but just in case).
DELETE FROM group_members WHERE username ~ '^lt_[0-9]+$';

-- 4) Push subscriptions (likewise defensive).
DELETE FROM push_subscriptions WHERE username ~ '^lt_[0-9]+$';

-- 5) The accounts themselves.
DELETE FROM users WHERE username ~ '^lt_[0-9]+$';

-- ── Verification: every count must be zero ─────────────────────────────────
\echo '=== After deletion (zeros expected) ==='
SELECT count(*) AS lt_users_left    FROM users     WHERE username ~ '^lt_[0-9]+$';
SELECT count(*) AS lt_messages_left FROM messages  WHERE sender ~ '^lt_[0-9]+$' OR recipient ~ '^lt_[0-9]+$';

COMMIT;

-- Refresh planner statistics after a bulk delete.
VACUUM ANALYZE messages;
VACUUM ANALYZE reactions;
