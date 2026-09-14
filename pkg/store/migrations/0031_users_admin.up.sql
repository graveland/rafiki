-- Exactly one binary distinction: an admin reviews every user's
-- conversations, everyone else only their own. A boolean, not a role enum --
-- nobody has defined a second role yet, and this migrates into a richer
-- scheme later without regret.
ALTER TABLE conversations.users
  ADD COLUMN is_admin BOOLEAN NOT NULL DEFAULT false;