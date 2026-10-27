# AutoSync fixes (is zip madhe sagle changes)

Files badalleli / navin:
- internal/commands/autosync_engine.go   — reconnect+retry, bot=destination / userbot=source, rules, filename replace, placeholders, link rewrite, dead-session, watchdog wiring
- internal/commands/autosync_guard.go    (NEW) — stall watchdog (AUTOSYNC_STALL_SEC, default 120)
- internal/commands/autosync_links.go    (NEW) — source msg links -> destination, username-in-URL rules, entity remap
- internal/commands/autosync_placeholder.go (NEW) — same-type fake media (jpg/png/mp4/mkv/webm/mp3/pdf)
- internal/commands/autosync_session.go  (NEW) — dead session detect, login notify, admin logout
- internal/commands/autosync_progress.go — progress touch/grace for watchdog
- internal/commands/autosync_ui.go       — destination resolved via BOT
- internal/commands/catalog.go           — Logout button for other accounts (admin)
- internal/commands/replace_rules.go     — longest-first rule order
- internal/db/db_autosync.go             — placeholder + link-pending collections
- internal/utils/helpers.go              — FloodWait notify hook

Env (optional): AUTOSYNC_TEXT_DELAY_MS=500, AUTOSYNC_MEDIA_DELAY_MS=1200, AUTOSYNC_STALL_SEC=120
- resume flag: restart/timeout/cap nantar task aapoaap continue (db.AutoSyncTask.Resume)
- auto_delivery.go + replace_ent.go: admin (global) rules compulsory in auto-delivery captions (entity-aware)
- progress/status: build tag AS-r7, rules loaded count, 'Replace lagla x/y msgs'
- /asfixrules [all|task_id], /asfixstop — destination madhlya junya messages var replace rules parat (edit), live progress (autosync_backfill.go)
- /asfixrules ata buttons (asfx:*): scope -> mode (⚡ Fast / 🐢 File name sahit re-upload) -> live progress + Stop. AutoSync menu madhe '🔧 Replace Backfill' button.
- contact gate: email step removed (mobile share = lagech registration), accepted-state reminder, first-contact admin notify (name/username/TG ID/pahila message), gate_contacts collection + startup sweep
- auto-delivery: restart nantar adaklela ('sending') lecture course 'done' mark honar nahi; startup var stale claims sodto; lease 5 min
- verified: full project go build OK (sqlite stubbed), go vet clean, logic tests pass (autosync_logic_test.go.example)
- bugfixes (found by tests): short @username New (@ak) ata link rewrite hoto; bare word New => link unlink; backfill scan 1000-empty rule; resume-loop guard (progress nasel tar auto-resume nahi); map (task,dest_msg_id) index
- real MongoDB integration tests (30+ tests) pass: live rule reload, link rewrite, resume flag, gate contacts (race fixed: unique tg_id index), AD claim/stale release, map paging/placeholders
- fix: gate_contacts race => duplicate 'navin user' admin notifications + duplicate docs
