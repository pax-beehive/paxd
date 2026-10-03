# Encrypted history text segments

The E2EE projector follows Manager's `text_layout: "segment"` contract.
Consecutive text deltas with the same turn and type share a message. Non-text
ACP frames, responses, and text-type or turn changes close that segment; later
text starts a distinct message. The layout marker is inside the encrypted header.
Only the current assistant segment retains its plaintext accumulation buffer.
Partial checkpoints still replace the entire unsealed part at a higher revision;
neither checkpoints nor transport batch boundaries start a new segment.

This keeps commentary, tools, and subsequent commentary in their original order
when Console restores canonical history. Existing turn-wide aggregated rows do
not contain enough information to reconstruct their original text boundaries.
The change applies to newly projected history after upgrading paxd.

## Whole-turn encrypted frame replay

The primary Console restore path reads the encrypted realtime journal, grouped
by the opaque `turn_ref` reliable-message metadata. This reference matches the
authenticated `turn_id` inside the encrypted event batch. It reveals turn
grouping, not prompt content or native request IDs. Manager indexes the reference
and returns ciphertext; it does not parse ACP.

Before dispatching a non-empty `session/prompt`, the bridge journals an encrypted
copy of the original command as an `acp_event`. This preserves encrypted attachment
references before local file substitution. Replies retain the existing per-frame
flow and 75 ms / 16 KiB batching. Flushes serialize to preserve order, and pending
output is flushed before changing the turn reference. Trailing usage/status
frames retain the last turn reference after the completion response.

Canonical message/part projection remains available for older consumers but is
not a prerequisite for replaying new turns. Its send failures do not block the
realtime event stream. No new atomic event-plus-history write is introduced.
Frames predating `turn_ref` remain a legacy unindexed bucket; Manager cannot
retroactively infer their turn boundaries without decrypting them.

Deploy Manager's replay API/schema first, then paxd, then Console. Existing
Manager versions ignore the new metadata, so they cannot index turns until
upgraded. New Console requires the JSON replay API. The runtime BDD tests cover
encrypted prompt/reply replay while canonical history writes fail, failed prompt
journaling, and trailing frames retaining their turn reference.
