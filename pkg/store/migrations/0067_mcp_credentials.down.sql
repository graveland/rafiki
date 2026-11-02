-- Inverse of 0067_mcp_credentials.up.sql. Grant tombstones and endpoint rows
-- are both owned by these two tables, so dropping them removes every object the
-- up migration added (including its indexes and CHECK constraints).
DROP TABLE IF EXISTS conversations.mcp_endpoint;
DROP TABLE IF EXISTS conversations.mcp_credential;
