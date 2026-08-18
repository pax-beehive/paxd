# Attachment localization behavior

## Scenario: the same attachment ID belongs to two remotes

Given two authenticated remotes reference the same attachment ID
When both remotes request localization
Then each remote gets independent in-memory state, partial data, ready metadata, and final content
And neither remote can query or receive the other remote's state

## Scenario: a local caller requests localization

Given the request source is explicitly local
When the attachment is localized
Then its cache is stored under the local scope
And no remote control WebSocket receives its state changes

## Scenario: an old unscoped cache exists

Given a legacy ready manifest exists directly under the attachment root
When a scoped caller requests that attachment
Then the legacy manifest is not reused
And a new scoped copy is downloaded and verified

## Scenario: a remote ID contains path syntax

Given a remote ID contains path separators or parent-directory syntax
When its attachment path is resolved
Then a SHA-256-derived directory name is used
And the localized file remains below the configured attachment root

## Scenario: a zero-byte attachment returns data

Given the manager declares an attachment size of exactly zero bytes
When the storage response contains one or more bytes
Then localization fails with `size_mismatch`
And no ready manifest is published
