ALTER TABLE conversations.executor_enrollment_token DROP COLUMN IF EXISTS owner_user_id;
ALTER TABLE conversations.executors DROP COLUMN IF EXISTS owner_user_id;
DROP TABLE IF EXISTS conversations.user_identity;
DROP TABLE IF EXISTS conversations.user_token;
DROP INDEX IF EXISTS conversations.users_email_active;
ALTER TABLE conversations.users DROP COLUMN IF EXISTS email;
