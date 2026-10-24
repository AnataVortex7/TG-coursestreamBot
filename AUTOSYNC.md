# AutoSync (source -> destination scheduled copy)

All control is from the admin bot (`/cmd` -> "Auto Sync" category, or the slash commands).

| Command | What it does |
|---|---|
| `/asaccount` | Add an extra userbot account (phone -> OTP -> 2FA). Does not touch the system userbot. |
| `/asadd` | Wizard: name -> userbot account -> source -> destination -> (topics) -> mode -> times -> replace words -> start point |
| `/autosync` | List tasks + status |
| `/asrun <id>` | Run now |
| `/astoggle <id>` | Pause / resume |
| `/astimes <id> 09:00, 21:30` | Change daily times (IST) |
| `/asreplace <id> old => new` | Task-specific replace words (multi-line ok, `-` clears). Global admin rules also apply. |
| `/asreset <id> <msgid>` | Move the source cursor (0 = from the beginning) |
| `/asdel <id> DELETE` | Delete a task |

Modes: **fast** = copy by media reference (no download; falls back to full on failure),
**full** = userbot download + re-upload (works for protected / no-forward chats).

No repeats: per-task cursor (`last_msg_id`) + unique (task, source msg) map + deterministic RandomID.
Topics: if source and destination are both forums, topics are created in the destination with the same title.

Env: `AUTOSYNC_MAX_PER_RUN` (default 2000 messages per run).

Limits: albums are copied as separate messages; edits/deletes in the source are not synced;
polls/games are skipped; the userbot must be a member of the source and an admin/poster in the destination.
