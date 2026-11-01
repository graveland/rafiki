-- The OpenRouter provider slug a turn's request was pinned to with
-- provider.only=[slug] (the guard's automatic pin: the provider that served the
-- conversation's previous turn). NULL means OpenRouter chose — an unpinned
-- request, a non-OpenRouter turn, or a turn captured before this column existed.
-- With served_provider it makes "asked for X, got an error" a query: a failed
-- attempt's own row names the provider it was forced onto.
ALTER TABLE conversations.conversation_turn ADD COLUMN pinned_provider text NULL;
