-- Set by the daemon when it accepts a /clear for a claude child; consumed by
-- capture's resolveHorizon on the first request whose head diverges from the
-- stored head, which it then records as a kind='clear' boundary.
ALTER TABLE conversations.conversation ADD COLUMN clear_pending BOOLEAN NOT NULL DEFAULT false;
