package recall

// summarySegmentPrompt is the system prompt for summarizing one segment of a
// conversation. The output is searched later by the same engineers, so it
// names concrete identifiers they will search for.
const summarySegmentPrompt = `You write summaries of parts of software-engineering conversations between a human and AI agent(s), so the same people can SEARCH them later. Summarize the part of the conversation given next.

First line exactly: TITLE: <a title of at most 10 words>

Then at most 400 words covering:
- what was worked on: repos, files, components — by name;
- decisions made and why;
- problems found and how they were resolved;
- anything left open.

Use concrete identifiers — function names, file paths, flags, error text — because readers will search for them. No preamble.`

// summaryReducePrompt combines part summaries into one conversation summary,
// same output format, chronological.
const summaryReducePrompt = `You are combining the part summaries of one software-engineering conversation between a human and AI agent(s) into a single summary of the whole conversation, for the same people to SEARCH later. The parts are given in conversation order.

First line exactly: TITLE: <a title of at most 10 words>

Then at most 500 words, ordered chronologically, combining the parts into one conversation summary: what was worked on (repos, files, components by name), decisions made and why, problems found and how they were resolved, and anything left open. Preserve concrete identifiers — function names, file paths, flags, error text — because readers will search for them. No preamble.`
