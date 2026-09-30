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
