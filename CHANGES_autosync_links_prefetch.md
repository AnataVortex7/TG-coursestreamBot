# AutoSync: summary links + download prefetch

## 1. Summary links (source -> destination)
- autosync_links.go (new): asLinkRewriter - text/caption madhle t.me/c/<source>/<msg> (ani /<topic>/<msg>)
  links => t.me/c/<destination>/<navin msg> (autosync_map madhun). Mirror topics ON asel ani topic mapped asel tar topic pan.
- autosync_engine.go: asTransformTextLinks (TextURL + plain URL entities, bold/formatting tasach). Links asleli text la link-preview band.
- Target ajun copy navhta (skip/fail) => original link tasach, run shevti summary destination madhe EDIT karun fix (asFixUnresolvedSummaries).
  Fix zala nahi tar status madhe warning.

## 2. Speed (prefetch)
- Current message upload hot astana pudhcha document download background madhe suru (userbotdl sarkha pipeline).
- Env: AUTOSYNC_PREFETCH=0 band, AUTOSYNC_PREFETCH_MAX_MB (default 2048), DL_THREADS / UP_THREADS (admin_stream.go che).
