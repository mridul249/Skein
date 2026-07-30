# Implementation notes

Things a reader of the code will wonder about, answered once here rather than
in a comment at every call site.

## Why the manifest exists from the first file

`file_shards` was created in Phase 3, when every file had exactly one shard and
striping was still two phases away.

A one-shard file and a many-shard file are the same shape: an ordered list of
(drive, provider object, plaintext range). Introducing that table later would
have meant migrating live rows into a schema the striping reader depends on,
and it would have meant the single-shard read path was never exercising the
code that the striped path uses. Making them the same from the start meant
Phase 5 changed the planner and nothing else.

## Why `plain_size_bytes` and `size_bytes` are separate

Ciphertext is longer than plaintext: a header plus one 16-byte tag per 64 KiB
frame. `size_bytes` is what the provider stores, `plain_size_bytes` is what the
user uploaded, and `plain_offset` is where the shard sits in the whole file.

Conflating them works exactly until encryption is enabled and then silently
mis-seeks every range request — a bug that produces plausible wrong bytes
rather than an error.

## Why uploads are a Reader chain, not a Writer chain

`storage.Backend.Put` takes an `io.Reader` and copies from it, because that is
what both the Drive resumable protocol and the filesystem want. An encrypting
`io.Writer` would need an `io.Pipe` and a goroutine to bridge the two, and a
pipe is one more place for a cancelled upload to leak a goroutine and a
half-open provider connection.

So the encrypter is a Reader that pulls: `Put` reads, which reads the
encrypter, which reads the multipart part, which reads the socket. One
consumer, no goroutines, and cancellation propagates by itself.

## Why the stored size has to be known before the first byte

Drive's resumable upload declares `X-Upload-Content-Length` when the session
opens, before any content is sent. So the ciphertext length must be computable
from the plaintext length rather than discovered while producing it — hence
`crypto.StreamOverhead`, and hence the framing being fixed-size rather than
compressed or padded to a variable boundary.

This is also why the planner takes an overhead function. A reservation that
covers the plaintext size runs the drive out of space at the final frame, and
that failure arrives after the entire file has been uploaded.

## Why nonces are derived rather than random

Per-file keys plus a `(shard_index, frame_index)` counter give a nonce that is
unique by construction. Random 96-bit nonces would have a birthday problem at
this frame count — a 30 GB file is roughly half a million frames — and, more
importantly, a random nonce has to be stored somewhere per frame, which would
break the fixed-stride offset arithmetic that range reads depend on.

## Why cleanup and reservation release use a detached context

The most common way an upload fails is that the client disappeared, which
cancels the request context. Cleanup running on that same context would skip
itself precisely when it is most needed, leaving orphan objects consuming quota
that nothing will ever reclaim.

Both run on `context.WithoutCancel` plus their own timeout.

## Why refresh-token reuse revokes the victim too

When a spent refresh token is presented, one of two things happened: it was
stolen, or the legitimate client replayed it. The server cannot tell which.
Revoking only the presented token would leave a thief's successor alive in the
stolen case, which is the worse failure — so the whole family goes, and the
legitimate user signs in again.

The family deadline also does not slide: a successor inherits its predecessor's
expiry. Otherwise a stolen chain that keeps refreshing lives forever, and
rotation costs an attacker nothing.

## Why `/healthz` and `/readyz` differ

`/healthz` touches nothing. A database outage does not mean the process is
broken, and an orchestrator that restarts on database failure turns a recovery
into a crash loop.

`/readyz` pings the database, so a rolling deploy waits for a process that can
actually serve.

## Why the streaming routes sit outside `/api`

Every JSON endpoint wants a 1 MiB `MaxBytesReader`. The upload endpoint wants
100 GiB. They cannot share a middleware group, so `/api/uploads` and
`/api/files/{id}/content` are mounted on the root router at their full paths,
with their own limits enforced in the handler.

## What is not implemented

Named because "missing" and "deliberately absent" look identical from outside:

- **Resumable uploads across a browser refresh.** The `uploads` table and its
  `plan` column exist for it; the handler does not.
- **Share links.** `share_links` is in the data model in `Architecture.md` and
  has no migration yet.
- **Multi-range requests.** Refused with 416 rather than half-implemented;
  they need `multipart/byteranges`, no browser media element sends one, and a
  wrong implementation corrupts downloads silently.
- **Content-addressed dedup.** `content_sha256` is recorded and indexed, so
  the data is there; nothing consumes it.
- **S3 / R2 / B2 backends.** `storage.Backend` is the whole interface needed;
  `KindS3` is already a valid value in the database constraint.
