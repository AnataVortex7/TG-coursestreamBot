# Changes: permanent user delete + admin-panel course send

## 1. Permanent delete (disable nahi)
- `internal/db/db_user_purge.go` (new) — `PurgeUserCompletely`: users, access, sessions, watch_history,
  stream_nonces, delivered_cache, sent_lecture_msgs, pending_msg_deletions, pending_deletes, auto_delivery_*,
  offline_tokens/usage, user_guard, tg_login_tokens, user_messages, otp_records/lockouts, login_attempts,
  pending_registrations, email_provider_map, relay_map, pending_course_sends. `users` doc LAST (retry-safe).
- `internal/db/cache.go` — per-user access/delivered cache invalidation.
- `internal/routes/admin_api.go` — `DELETE /api/admin/users/:id` ata hard delete. Admin/userbot docs protected.
  Aadhi bot-sent lectures user chya Telegram chat madhun kadhto (best-effort, 25s cap).
- `internal/commands/contact_gate.go` — `ForgetUserBotState` (gate "linked" cache pusto => user parat msg takla ki
  Verification popup -> number share -> naya registration).
- `internal/commands/janitor.go` — `ClearScheduledChatMsgs`.
- `web/admin_panel.html` — "Disable" button => "Delete" (permanent confirm).

## 2. Admin panel varun course send + registration nantar auto-send
- `POST /api/admin/users/:id/send-course` + Grant Access madhe "Telegram var send kara" checkbox.
- `internal/commands/send_course_queue.go` (new):
  - user linked + bot ne chat baghitla => lagech send.
  - nahi => `pending_course_sends` queue; `StartPendingCourseSend` worker (20s) user ne bot var msg takun
    Verification (number share, mobile DB madhe match => auto-link) purna kela ki apoaap pathavto.
- `cmd/fsb/run.go` — worker start.
- `internal/commands/auto_delivery_prompt.go` — queued send asel tar "sagle pathavu ka?" popup nahi (duplicate tala).

## 3. Raw-first, watermark-replace
`runCourseSend` ata `deliverLectureCoreOpt(..., cachedOnly=true)` vaprto: cache nasel tar original copy lagech
(ADStateRaw), nantar predownload ne LOG cache bharla ki `adResolvePending` message edit karto.
(Requires AUTO_DELIVERY=true.)

## 4. Bot /deluser + menu "Delete User" pan permanent
`internal/commands/user_delete.go` — shared `DeleteUserPermanently` (admin panel + bot dogh vaparatat).
`/deluser <mobile|email|username|tgid> DELETE`; menu madhe confirm step (DELETE lihā).

## 5. Bot /sendcourse = admin panel logic
`/sendcourse <mobile|email|tgid> <courseID> [from] [to]` ata `SendCourseRange` vaparto (queue + raw-then-replace).
Queue madhe from/to pan save hotat.
