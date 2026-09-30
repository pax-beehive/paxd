# Encrypted user attachments

Browser and paxd share the existing paired root key. Attachment v1 derives
32 bytes using HKDF-SHA256, empty salt, and this UTF-8 info tuple:
`pax/e2ee/v1/attachment\0{agent_id}\0{session_id}\0{key_epoch}\0{attachment_id}`.
The attachment ID is freshly random for every new encryption operation.

Files are concatenated binary AES-256-GCM chunks of 4 MiB plaintext plus 16-byte
tags. An empty file has one empty authenticated chunk. Each nonce is a 12-byte
big-endian chunk index. AAD is the compact JSON array:
`[1,"pax/e2ee/attachment",agent_id,session_id,String(key_epoch),attachment_id,size_bytes,chunk_count,index]`.
The shared browser/Go vector uses root byte 7 repeated 32 times, agent_1,
session_1, epoch 1, file_1, and plaintext hello. Expected ciphertext hex:
`c7b43e3b2c8d2c8c3889e4fd64e437d0710b4bdbbd`.

The authenticated session/prompt params contain paxEncryptedAttachments. Each
entry carries version, agent_id, session_id, key_epoch, attachment_id, filename,
content_type, size_bytes, chunk_count, object_id and download_url. The bridge
checks the descriptor against the enclosing envelope before making any download.
Only HTTPS URLs are accepted, redirects are rejected, request errors are redacted,
and downloads have a ten-minute deadline. Limits are 512 MiB/file and 16 files.
All chunks and EOF must authenticate before a file is renamed out of staging.
Failures remove staging files and return an encrypted RPC error without dispatch.
Successful files use private directories and 0600 permissions beneath
~/.paxd/e2ee-attachments, scoped by a hash of agent/session. Files are retained for
subsequent agent reads; lifecycle garbage collection is not yet implemented.

Only resource_link blocks reach ACP and encrypted history; the download URL and
descriptor are removed. No ordinary control-channel attachment state is published.
Browser file preview and resumable transfer remain separate follow-up work.
