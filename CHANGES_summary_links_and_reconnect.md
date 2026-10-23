# Changes: summary deep-links + "engine closed" fix

## 1. Summary msg (topic list) — send channel
- `internal/commands/summary_links.go` (new)
  - text msg aata Phase B madhe jato (ordering sathi) ani **entities (bold/link) preserve** hotat.
  - `t.me/c/<source>/<msgID>` links => `t.me/c/<sendChannel>/<navin msgID>` (DB map: `userbotdl_msgmap`).
  - Replace-rules piece-wise lagtat, entity offsets (UTF-16) parat calculate hotat.
  - Link cha target copy zalela nasel => summary `unresolved` mark hoto, job shevti send channel madhe EDIT karun fix karto.
- `internal/db/db_msgmap.go` (new): `userbotdl_msgmap`, `userbotdl_summaries`, `lecture_msg_map`.
- `admin_stream.go`: media send cha error aata ignore hot nahi + source->dest msgID save.

## 2. Course user la dyaychya veli summary
- `catalog.go`: 3 delivery points var `RecordLectureMsgMap` (lecture msgID -> user chya chat madhla bot cha msgID).
- `send_course_queue.go`: `runCourseSend` shevti `sendCourseSummaries` — links = `tg://openmessage?user_id=<bot>&message_id=<id>`;
  user la na gelelya lecture cha link kadhun takto (channel link leak nahi).

## 3. "rpcDoRequest: engine was closed / forcibly closed"
- `internal/commands/userbot_resilience.go` (new): transient error detect, client ping, marlela cached client drop + revive.
- `runUserbotDL`: survatila client ping; `runPhaseA` madhe connection errors sathi 8 retries (backoff) + client refresh;
  shevti connection-error mule fail zalelya msgs sathi 1 retry pass.
