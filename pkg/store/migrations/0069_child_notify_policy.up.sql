-- notify_policy: '' = the spawner hears every settle (the default); 'result' = the spawner hears only settles that carry a result, question, error, own-cap breach or exit
ALTER TABLE conversations.child ADD COLUMN notify_policy TEXT NOT NULL DEFAULT '' CHECK (notify_policy IN ('', 'result'));
