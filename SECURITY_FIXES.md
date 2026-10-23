# Security & bug fixes

> No Go toolchain was available when these edits were made. Run
> `go build ./... && go vet ./cmd/... ./internal/... ./config/...` before deploying.

## Critical
1. **Account takeover via Telegram deep-link** (`/login/otp/send` -> `totp_` link). Anyone knowing a
   user's mobile/email could link THEIR Telegram to the account and receive its OTP.
   Fix: `contact_gate.go` no longer lets `totp_` bypass phone verification; `start.go`
   never binds a Telegram ID from a token (only the verified-phone gate links).
2. **Spoofable client IP** (X-Forwarded-For trusted from everyone) -> admin brute-force limiter and
   `ADMIN_ALLOWED_IPS` could be bypassed. Fix: `SetTrustedProxies` + env `TRUSTED_PROXIES`.
3. **Admin panel stored XSS**: user-controlled `full_name`/email inside `onclick="fn('${esc(x)}')"`
   (HTML entity is decoded before JS runs). Fix: `jsq()` helper (19 sites). Also repairs the broken
   "Kill session" button.
4. **Admin login limiter** was racy (parallel requests passed the check) and unbounded in memory.
   Rewritten: attempts counted up-front under a mutex; SameSite=Strict admin cookie.

## High
5. Token type confusion: one AES key for all tokens -> a /watch token worked on the unauthenticated
   /stream endpoint. Fix: AES-GCM AAD domain separation (worker tokens unchanged for worker compat).
6. `fsb.env` was read AFTER `auth`/`token`/`email` `init()` -> JWT/STREAM/SMTP settings in the file
   were ignored/crashed. Fix: `internal/envinit`.
7. Placeholder secrets (`change-me...`) were accepted. Now rejected; STREAM_SECRET must be >= 16 chars.
8. OTP/password attempt limits raced under parallel requests -> per-identifier lock.
9. Email header (CRLF) injection via registration email in Subject/To; unescaped HTML in
   purchase/contact/OTP emails; SMTP had no deadline.
10. Admin password reset did not end existing sessions.

## Medium / low
- Access logs contained stream/watch tokens -> redacted. HTTP server had no timeouts (Slowloris).
- Public `/stream` could serve html/svg inline from the site origin; Content-Disposition built unsafely.
- `crypto/rand` for offline tokens; constant-time COLAB secret compare; worker URL validation;
  cooldown + size caps on contact/purchase/device-notify; login timing/enumeration reduced;
  registration validation (email/mobile/length, password >= 8).
- config.go: panic on `MULTI_TOKEN*` env, `--log-channel` flag ignored, wrong `stripInt`.
- Stray build-breaking files (`fix*.go`, `scratch*`, root `main.go`, `handler.go`, `botpool/`,
  `registration/`, `token/`) tagged `//go:build ignore`; `.gitignore` added.

## Deploy notes
- Existing `/watch`, `/stream` links and file keys become invalid once (new AAD) - users just reload.
- Set `TRUSTED_PROXIES` if you are behind Cloudflare / a proxy with public IPs (else all users share one IP).
- Rotate JWT_SECRET/STREAM_SECRET if they were ever the sample value.
- Unlinked users now log in with: Telegram bot -> share phone -> back to site -> request OTP.

## Not changed (your decision)
- Admin password-change email contains the new password in plain text.
- Lockout keyed by identifier lets anyone lock a victim out (4 wrong tries).
- Device lock = OS+resolution+timezone (client-supplied, easy to fake); 5-day delete request sends no alert.
- `STREAM_SECRET` is shared with workers (a compromised worker can forge tokens); admin session list shows full JWTs.
- CSP still allows `unsafe-inline`/`unsafe-eval`; admin has no 2FA; URL monitor can fetch internal URLs (SSRF, admin-only).
- Delete leftovers: `diff.txt`, `internal - Shortcut.lnk`, `bin/fsb.exe` (LFS pointer).
