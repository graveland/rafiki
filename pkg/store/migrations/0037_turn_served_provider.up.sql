-- Which OpenRouter provider actually served each captured turn (e.g. "Together",
-- "Parasail"). OpenRouter load-balances every call across many backends with
-- different quantizations, so the turn's upstream ("openrouter") alone cannot
-- separate model effects from provider effects. Written by capture's
-- CompleteTurn from the response body's top-level "provider" field; NULL means
-- not reported (a native Anthropic response, or a turn captured before this
-- column existed). Nullable and default-less on purpose: absent is the only
-- honest value for a turn the provider never named.
ALTER TABLE conversations.conversation_turn ADD COLUMN served_provider text NULL;
