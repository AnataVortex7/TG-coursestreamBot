// internal/email/brevo.go
// Fixed-assignment multi-SMTP email sender - ROUND ROBIN NAHI.
// Pratyek recipient email la ekach SMTP provider PERMANENTLY assign hoto
// (DB madhe save - internal/db madhle email_provider_map collection).
// Index 0 (UNSUFFIXED env vars - BREVO_SMTP_USER, BREVO_FROM_EMAIL, no
// numeric suffix) = DEDICATED OVERFLOW provider, unlimited capacity, FAKT
// tevha vaaparla jato jevha sagle numbered providers (1..n-1, mhanje _2,
// _3, _4...) full astil (pratyekala max 7 emails capacity). Navin numbered
// SMTP (udaharan _4) konthyahi velī add kela tari overflow cha role index 0
// kadech fixed rahato - "last configured one" var depend nahi.
// Ekda assign zala ki to email kayam toच provider vaaparel - admin
// sakat sagle recipients ya same logic madhe yetat, vegla path nahi.
// Overflow provider (unsuffixed) sathi BREVO_SMTP_USER/BREVO_SMTP_PASS/
// BREVO_FROM_EMAIL env vars vapra. Numbered capacity providers sathi
// BREVO_SMTP_USER_2/_3/_4..., tyaच prakare PASS ani FROM_EMAIL.
package email

import (
	"EverythingSuckz/fsb/internal/db"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// ─── SMTP Config ──────────────────────────────────────────────────────────────

type smtpConfig struct {
	host      string
	port      string
	user      string
	pass      string
	fromEmail string
	fromName  string
}

var smtpProviders []smtpConfig

func init() {
	for i := 1; ; i++ {
		suffix := ""
		if i > 1 {
			suffix = fmt.Sprintf("_%d", i)
		}
		user := os.Getenv("BREVO_SMTP_USER" + suffix)
		pass := os.Getenv("BREVO_SMTP_PASS" + suffix)
		if user == "" || pass == "" {
			break
		}
		host := envOr("BREVO_SMTP_HOST"+suffix, "smtp-relay.brevo.com")
		smtpProviders = append(smtpProviders, smtpConfig{
			host:      host,
			port:      envOr("BREVO_SMTP_PORT"+suffix, "587"),
			user:      user,
			pass:      pass,
			fromEmail: envOr("BREVO_FROM_EMAIL"+suffix, user),
			fromName:  envOr("BREVO_FROM_NAME"+suffix, envOr("BREVO_FROM_NAME", "Akshayclasses")),
		})
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ─── Health check ─────────────────────────────────────────────────────────────

func IsServiceUp() bool {
	for _, p := range smtpProviders {
		if dialAndAuth(p) {
			return true
		}
	}
	return false
}

func dialAndAuth(p smtpConfig) bool {
	if p.user == "" || p.pass == "" {
		return false
	}
	rawConn, err := net.DialTimeout("tcp", p.host+":"+p.port, 5*time.Second)
	if err != nil {
		return false
	}
	client, err := smtp.NewClient(rawConn, p.host)
	if err != nil {
		return false
	}
	defer client.Close()

	tlsCfg := &tls.Config{ServerName: p.host, MinVersion: tls.VersionTLS12}
	if err = client.StartTLS(tlsCfg); err != nil {
		return false
	}
	auth := smtp.PlainAuth("", p.user, p.pass, p.host)
	if err = client.Auth(auth); err != nil {
		log.Printf("[SMTP] Auth failed for %s: %v", p.host, err)
		return false
	}
	return true
}

// ─── Core send — FIXED assignment, NO round-robin, NO fallback ────────────────
//
// maxPerProvider — pratyek provider (last sodun) la max kiti emails permanently
// assign karaycha, tyanantar पुढच्या provider kade jaते.
const maxPerProvider = 7

// pickProviderForRecipient - email la aadhich provider assign asel tar toч
// return karto. Nasel tar: numbered providers (BREVO_SMTP_USER_2, _3, ... -
// indices 1..n-1) madhe (sequence ne) jyachi capacity (maxPerProvider) bharleli
// nahi to provider nivadto. SAGLE numbered providers full astil tarach index 0
// (unsuffixed BREVO_SMTP_USER/BREVO_FROM_EMAIL - DEDICATED OVERFLOW, unlimited)
// vaaparla jail. Mag to assignment DB madhe PERMANENTLY save karto.
func pickProviderForRecipient(email string) (int, error) {
	n := len(smtpProviders)
	if n == 0 {
		return 0, fmt.Errorf("smtp: no providers configured")
	}
	if n == 1 {
		return 0, nil // ekach provider aahe - overflow/capacity cha question nahi
	}

	// Aadhich permanently assign zalay ka check kar
	if idx, found, err := db.GetEmailProviderAssignment(email); err == nil && found {
		if idx >= 0 && idx < n {
			return idx, nil
		}
		// Saved index aata valid providers range cha baher gela (providers
		// kami zale astil) - overflow (index 0) var fallback.
		return 0, nil
	}

	// Navin email - capacity-based assignment. Index 0 (unsuffixed
	// BREVO_SMTP_USER / BREVO_FROM_EMAIL = DEDICATED OVERFLOW) sodun, numbered
	// providers (1..n-1, mhanje _2, _3, _4...) madhe jaga aahe ka baghto
	// (sequence ne: _2 pahile fill, mag _3, mag _4...). Navin numbered SMTP
	// nantar add zala tari overflow cha role index 0 kadech fixed rahato -
	// "last provider" var depend nahi.
	chosen := 0 // default: overflow (index 0, unsuffixed - sagle numbered full astil tevhach vaaparla jail)
	for i := 1; i < n; i++ {
		count, err := db.CountEmailsForProvider(i)
		if err != nil {
			continue // DB issue - pudhcha provider try kar
		}
		if count < maxPerProvider {
			chosen = i
			break
		}
	}

	if err := db.AssignEmailProvider(email, chosen); err != nil {
		log.Printf("[SMTP] Warning: assignment save failed for %s: %v", email, err)
	}

	// Race-condition safety: don goroutines ekach navin email sathi ekach vele
	// assignment karat astil tar, DB cha upsert (unique index) fakt EK record
	// thevel. Authoritative value re-fetch karun tich vaapraychi, navhe to
	// localpane compute kelela `chosen` (jo "loser" goroutine sathi chukicha
	// asu shakto).
	if idx, found, err := db.GetEmailProviderAssignment(email); err == nil && found {
		return idx, nil
	}
	return chosen, nil
}

// sendMail — recipient cha permanently assigned provider shodhto (navin
// asel tar capacity-based navin assignment karто) ani तोच provider vaaparून
// pathvto. ROUND ROBIN NAHI. FALLBACK NAHI — assigned provider fail zala
// tar error तसाच return hoто, dusra provider try hot nahi (jaणूनबुजून — ekach
// SMTP cha credentials/reputation consistently त्याच users sathi rahaव).
func sendMail(toEmail, subject, htmlBody string) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}

	idx, err := pickProviderForRecipient(toEmail)
	if err != nil {
		return err
	}

	p := smtpProviders[idx]
	msg := buildMsg(p, toEmail, subject, htmlBody)
	if err := sendViaProvider(p, toEmail, msg); err != nil {
		log.Printf("[SMTP] Provider #%d (%s) failed for %s: %v", idx+1, p.host, toEmail, err)
		return fmt.Errorf("smtp: send failed via assigned provider #%d: %w", idx+1, err)
	}
	log.Printf("[SMTP] Sent via permanently-assigned provider #%d (%s) → %s", idx+1, p.host, toEmail)
	return nil
}

func sendViaProvider(p smtpConfig, toEmail string, msg []byte) error {
	addr := p.host + ":" + p.port
	rawConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	client, err := smtp.NewClient(rawConn, p.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	tlsCfg := &tls.Config{ServerName: p.host, MinVersion: tls.VersionTLS12}
	if err = client.StartTLS(tlsCfg); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}
	auth := smtp.PlainAuth("", p.user, p.pass, p.host)
	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err = client.Mail(p.fromEmail); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err = client.Rcpt(toEmail); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return w.Close()
}

// ─── Build raw MIME message ───────────────────────────────────────────────────

func buildMsg(p smtpConfig, toEmail, subject, htmlBody string) []byte {
	return []byte(
		"From: " + p.fromName + " <" + p.fromEmail + ">\r\n" +
			"To: " + toEmail + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n" +
			"\r\n" +
			htmlBody,
	)
}

func primaryProvider() smtpConfig {
	if len(smtpProviders) > 0 {
		return smtpProviders[0]
	}
	return smtpConfig{fromName: "Akshayclasses"}
}

// ─── Send OTP ─────────────────────────────────────────────────────────────────

func SendOTP(toEmail, otpCode string) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	p := primaryProvider()
	subject := "Your Login OTP — " + p.fromName
	htmlBody := buildOTPEmail(p.fromName, otpCode)
	return sendMail(toEmail, subject, htmlBody)
}

// ─── Send Registration OTP ────────────────────────────────────────────────────

func SendRegOTP(toEmail, fullName, otpCode string) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	p := primaryProvider()
	subject := "Verify your email — " + p.fromName
	htmlBody := buildRegOTPEmail(p.fromName, fullName, otpCode)
	return sendMail(toEmail, subject, htmlBody)
}

// ─── Send Purchase Notification ───────────────────────────────────────────────

func SendPurchaseNotification(adminEmail string, info PurchaseInfo) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	if adminEmail == "" {
		return fmt.Errorf("smtp: ADMIN_EMAIL not set")
	}
	subject := fmt.Sprintf("🎁 New Purchase Request — %s", info.CourseTitle)
	htmlBody := buildPurchaseEmail(info)
	return sendMail(adminEmail, subject, htmlBody)
}

// ═══════════════════════════════════════════════════════════════════════════════
// TYPES
// ═══════════════════════════════════════════════════════════════════════════════

type PurchaseInfo struct {
	UserFullName string
	UserMobile   string
	UserEmail    string
	Username     string
	CourseTitle  string
	CoursePrice  string
	CourseID     string
	CardNumber   string
	CardPIN      string
	Message      string
}

// ─── Email Templates ──────────────────────────────────────────────────────────

func buildRegOTPEmail(appName, fullName, otp string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;margin:0;padding:40px 16px;">
  <div style="max-width:460px;margin:0 auto;background:#1a1d27;border-radius:16px;padding:40px 32px;border:1px solid #2a2d3a;">
    <div style="text-align:center;margin-bottom:32px;">
      <div style="width:60px;height:60px;background:#6c63ff;border-radius:16px;display:inline-flex;align-items:center;justify-content:center;margin-bottom:16px;">
        <span style="font-size:28px;">🎓</span>
      </div>
      <h1 style="color:#e8e9f0;font-size:22px;font-weight:700;margin:0 0 6px;">Verify your email</h1>
      <p style="color:#7a7d8e;font-size:14px;margin:0;">Hi %s, welcome to %s!</p>
    </div>
    <div style="text-align:center;margin:28px 0;">
      <p style="color:#7a7d8e;font-size:14px;margin-bottom:14px;">Your verification code</p>
      <div style="display:inline-block;letter-spacing:14px;font-size:44px;font-weight:800;color:#6c63ff;font-family:'Courier New',monospace;background:#12141c;padding:20px 28px;border-radius:14px;border:2px solid #2a2d3a;">%s</div>
      <p style="color:#7a7d8e;font-size:13px;margin-top:14px;">⏱ Valid for <strong style="color:#e8e9f0;">10 minutes</strong> · Max 3 attempts</p>
    </div>
    <div style="background:#12141c;border-radius:10px;padding:14px 18px;text-align:center;margin-top:8px;">
      <p style="color:#7a7d8e;font-size:12px;margin:0;line-height:1.6;">
        If you didn't create an account, ignore this email.<br>
        <strong style="color:#ff5c5c;">Never share this code with anyone.</strong>
      </p>
    </div>
  </div>
</body>
</html>`, fullName, appName, otp)
}

func buildOTPEmail(appName, otp string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;margin:0;padding:40px 16px;">
  <div style="max-width:460px;margin:0 auto;background:#1a1d27;border-radius:16px;padding:40px 32px;border:1px solid #2a2d3a;">
    <div style="text-align:center;margin-bottom:32px;">
      <div style="width:60px;height:60px;background:#6c63ff;border-radius:16px;display:inline-flex;align-items:center;justify-content:center;margin-bottom:16px;">
        <span style="font-size:28px;">🎓</span>
      </div>
      <h1 style="color:#e8e9f0;font-size:22px;font-weight:700;margin:0 0 6px;">Login OTP</h1>
      <p style="color:#7a7d8e;font-size:14px;margin:0;">%s</p>
    </div>
    <div style="text-align:center;margin:28px 0;">
      <p style="color:#7a7d8e;font-size:14px;margin-bottom:14px;">Your one-time password</p>
      <div style="display:inline-block;letter-spacing:14px;font-size:44px;font-weight:800;color:#6c63ff;font-family:'Courier New',monospace;background:#12141c;padding:20px 28px;border-radius:14px;border:2px solid #2a2d3a;">%s</div>
      <p style="color:#7a7d8e;font-size:13px;margin-top:14px;">⏱ Valid for <strong style="color:#e8e9f0;">10 minutes</strong> · Max 3 attempts</p>
    </div>
    <div style="background:#12141c;border-radius:10px;padding:14px 18px;text-align:center;margin-top:8px;">
      <p style="color:#7a7d8e;font-size:12px;margin:0;line-height:1.6;">
        If you didn't request this, ignore this email.<br>
        <strong style="color:#ff5c5c;">Never share this code with anyone.</strong>
      </p>
    </div>
  </div>
</body>
</html>`, appName, otp)
}

func buildPurchaseEmail(info PurchaseInfo) string {
	identifier := info.UserMobile
	if identifier == "" {
		identifier = info.UserEmail
	}
	if identifier == "" {
		identifier = info.Username
	}
	grantCmd := fmt.Sprintf("/grantaccess %s %s 0", identifier, info.CourseID)

	msgRow := ""
	if info.Message != "" {
		msgRow = fmt.Sprintf(`
      <tr>
        <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;white-space:nowrap;">💬 Message</td>
        <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:13px;">%s</td>
      </tr>`, info.Message)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;margin:0;padding:32px 16px;">
  <div style="max-width:540px;margin:0 auto;background:#1a1d27;border-radius:16px;border:1px solid #2a2d3a;overflow:hidden;">
    <div style="background:#6c63ff;padding:24px 28px;text-align:center;">
      <div style="font-size:36px;margin-bottom:8px;">🎁</div>
      <h1 style="color:#fff;font-size:20px;font-weight:700;margin:0 0 4px;">New Purchase Request</h1>
      <p style="color:rgba(255,255,255,.8);font-size:14px;margin:0;">Gift card payment received</p>
    </div>
    <div style="padding:24px 28px;">
      <h3 style="color:#a5b4fc;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">👤 User Information</h3>
      <table style="width:100%%;border-collapse:collapse;background:#12141c;border-radius:10px;overflow:hidden;margin-bottom:20px;">
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;white-space:nowrap;">Full Name</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:13px;font-weight:600;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;">📱 Mobile</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:13px;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;">📧 Email</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:13px;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;color:#7a7d8e;font-size:13px;">👤 Username</td>
          <td style="padding:10px 16px;color:#e8e9f0;font-size:13px;font-family:monospace;">%s</td>
        </tr>
      </table>
      <h3 style="color:#a5b4fc;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">📚 Course Information</h3>
      <table style="width:100%%;border-collapse:collapse;background:#12141c;border-radius:10px;overflow:hidden;margin-bottom:20px;">
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;">Course</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:13px;font-weight:600;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;">💰 Price</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#22c55e;font-size:13px;font-weight:700;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;color:#7a7d8e;font-size:13px;">Course ID</td>
          <td style="padding:10px 16px;color:#7a7d8e;font-size:11px;font-family:monospace;">%s</td>
        </tr>
      </table>
      <h3 style="color:#a5b4fc;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">🎁 Gift Card Details</h3>
      <table style="width:100%%;border-collapse:collapse;background:#12141c;border-radius:10px;overflow:hidden;margin-bottom:20px;">
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;white-space:nowrap;">Card Number</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:15px;font-weight:700;font-family:monospace;letter-spacing:2px;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;white-space:nowrap;">PIN</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:15px;font-weight:700;font-family:monospace;letter-spacing:4px;">%s</td>
        </tr>
        %s
      </table>
      <h3 style="color:#4ade80;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">✅ Grant Access Command</h3>
      <div style="background:#0d1f12;border:1.5px solid rgba(34,197,94,.35);border-radius:10px;padding:16px 18px;">
        <p style="color:#7a7d8e;font-size:11px;margin:0 0 8px;">Bot madhe ha command paste kara:</p>
        <code style="color:#4ade80;font-size:14px;font-weight:700;font-family:'Courier New',monospace;word-break:break-all;">%s</code>
      </div>
      <p style="color:#7a7d8e;font-size:11px;margin:16px 0 0;text-align:center;">
        Gift card verify karin access activate kara · Akshayclasses Admin Panel
      </p>
    </div>
  </div>
</body>
</html>`,
		info.UserFullName,
		orDash(info.UserMobile),
		orDash(info.UserEmail),
		info.Username,
		info.CourseTitle,
		orDash(info.CoursePrice),
		info.CourseID,
		info.CardNumber,
		info.CardPIN,
		msgRow,
		grantCmd,
	)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ─── Send Admin → User Message ────────────────────────────────────────────────

func SendAdminToUserMessage(toEmail, fullName, subject, message, appName string) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	p := primaryProvider()
	if appName == "" {
		appName = p.fromName
	}
	htmlBody := buildAdminToUserEmail(appName, fullName, subject, message)
	return sendMail(toEmail, subject, htmlBody)
}

func buildAdminToUserEmail(appName, fullName, subject, message string) string {
	htmlMessage := ""
	for _, line := range splitLines(message) {
		if line == "" {
			htmlMessage += "<br>"
		} else {
			htmlMessage += "<p style=\"color:#c8cad8;font-size:14px;line-height:1.7;margin:0 0 8px;\">" + escapeHTML(line) + "</p>"
		}
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;margin:0;padding:40px 16px;">
  <div style="max-width:520px;margin:0 auto;background:#1a1d27;border-radius:16px;border:1px solid #2a2d3a;overflow:hidden;">
    <div style="background:linear-gradient(135deg,#6c63ff,#4f46e5);padding:28px 32px;text-align:center;">
      <div style="font-size:38px;margin-bottom:10px;">📩</div>
      <h1 style="color:#fff;font-size:20px;font-weight:700;margin:0 0 4px;">Message from %s</h1>
      <p style="color:rgba(255,255,255,.75);font-size:13px;margin:0;">Hi %s, you have a new message</p>
    </div>
    <div style="padding:28px 32px;">
      <div style="background:#12141c;border-left:3px solid #6c63ff;border-radius:0 10px 10px 0;padding:18px 20px;margin-bottom:20px;">
        <p style="color:#7a7d8e;font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">📋 Subject</p>
        <p style="color:#e8e9f0;font-size:15px;font-weight:600;margin:0;">%s</p>
      </div>
      <div style="background:#12141c;border-radius:10px;padding:20px 22px;margin-bottom:20px;">
        <p style="color:#7a7d8e;font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 12px;">💬 Message</p>
        %s
      </div>
      <div style="text-align:center;padding:10px 0;">
        <p style="color:#4a4d5e;font-size:11px;margin:0;">This message was sent by the %s admin team.</p>
      </div>
    </div>
  </div>
</body>
</html>`, appName, escapeHTML(fullName), escapeHTML(subject), htmlMessage, appName)
}

// ─── Send User → Admin Contact Message ───────────────────────────────────────

type UserContactInfo struct {
	UserFullName string
	UserMobile   string
	UserEmail    string
	Username     string
	Subject      string
	Message      string
}

func SendUserContactToAdmin(adminEmail string, info UserContactInfo) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	if adminEmail == "" {
		return fmt.Errorf("smtp: ADMIN_EMAIL not set")
	}
	p := primaryProvider()
	subject := fmt.Sprintf("📬 New Message from %s — %s", info.UserFullName, info.Subject)
	htmlBody := buildUserContactEmail(p.fromName, info)
	return sendMail(adminEmail, subject, htmlBody)
}

func buildUserContactEmail(appName string, info UserContactInfo) string {
	htmlMessage := ""
	for _, line := range splitLines(info.Message) {
		if line == "" {
			htmlMessage += "<br>"
		} else {
			htmlMessage += "<p style=\"color:#c8cad8;font-size:14px;line-height:1.7;margin:0 0 8px;\">" + escapeHTML(line) + "</p>"
		}
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;margin:0;padding:32px 16px;">
  <div style="max-width:540px;margin:0 auto;background:#1a1d27;border-radius:16px;border:1px solid #2a2d3a;overflow:hidden;">
    <div style="background:linear-gradient(135deg,#059669,#047857);padding:24px 28px;text-align:center;">
      <div style="font-size:36px;margin-bottom:8px;">📬</div>
      <h1 style="color:#fff;font-size:20px;font-weight:700;margin:0 0 4px;">User Contact Message</h1>
      <p style="color:rgba(255,255,255,.8);font-size:13px;margin:0;">%s kडून navi message aali ahe</p>
    </div>
    <div style="padding:24px 28px;">
      <h3 style="color:#a5b4fc;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">👤 User Information</h3>
      <table style="width:100%%;border-collapse:collapse;background:#12141c;border-radius:10px;overflow:hidden;margin-bottom:20px;">
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;white-space:nowrap;">Full Name</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:13px;font-weight:600;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;">📱 Mobile</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:13px;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;">📧 Email</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:13px;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;color:#7a7d8e;font-size:13px;">👤 Username</td>
          <td style="padding:10px 16px;color:#e8e9f0;font-size:13px;font-family:monospace;">%s</td>
        </tr>
      </table>
      <h3 style="color:#a5b4fc;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">📋 Subject</h3>
      <div style="background:#12141c;border-left:3px solid #6c63ff;border-radius:0 10px 10px 0;padding:14px 18px;margin-bottom:20px;">
        <p style="color:#e8e9f0;font-size:14px;font-weight:600;margin:0;">%s</p>
      </div>
      <h3 style="color:#a5b4fc;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">💬 Message</h3>
      <div style="background:#12141c;border-radius:10px;padding:18px 20px;margin-bottom:20px;">
        %s
      </div>
      <p style="color:#4a4d5e;font-size:11px;text-align:center;margin:0;">%s Admin Panel · User Contact</p>
    </div>
  </div>
</body>
</html>`,
		escapeHTML(info.UserFullName),
		escapeHTML(info.UserFullName),
		orDash(info.UserMobile),
		orDash(info.UserEmail),
		info.Username,
		escapeHTML(info.Subject),
		htmlMessage,
		appName,
	)
}

// ─── HTML helpers ─────────────────────────────────────────────────────────────

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

func splitLines(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// ─── Send New Course Notification ────────────────────────────────────────────

func SendNewCourseNotification(toEmail, fullName, courseTitle, buyLink, price, appName string) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	p := primaryProvider()
	if appName == "" {
		appName = p.fromName
	}
	subject := "🎓 New Course Available — " + courseTitle
	htmlBody := buildNewCourseEmail(appName, fullName, courseTitle, buyLink, price)
	return sendMail(toEmail, subject, htmlBody)
}

func buildNewCourseEmail(appName, fullName, courseTitle, buyLink, price string) string {
	greet := "Hi there"
	if fullName != "" {
		greet = "Hi " + escapeHTML(fullName)
	}
	buyBtn := ""
	if buyLink != "" {
		buyBtn = fmt.Sprintf(`
      <div style="text-align:center;margin-top:24px;">
        <a href="%s" style="display:inline-block;background:linear-gradient(135deg,#6c63ff,#4f46e5);color:#fff;text-decoration:none;padding:14px 36px;border-radius:12px;font-size:15px;font-weight:700;letter-spacing:.3px;">
          🚀 View Course
        </a>
      </div>`, escapeHTML(buyLink))
	}
	priceTag := ""
	if price != "" {
		priceTag = fmt.Sprintf(`<span style="display:inline-block;background:#22c55e20;color:#4ade80;border:1px solid #22c55e40;border-radius:20px;padding:4px 14px;font-size:13px;font-weight:700;margin-top:8px;">%s</span>`, escapeHTML(price))
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;margin:0;padding:40px 16px;">
  <div style="max-width:500px;margin:0 auto;background:#1a1d27;border-radius:16px;border:1px solid #2a2d3a;overflow:hidden;">
    <div style="background:linear-gradient(135deg,#4f46e5,#7c3aed);padding:32px 28px;text-align:center;">
      <div style="font-size:48px;margin-bottom:12px;">🎓</div>
      <h1 style="color:#fff;font-size:22px;font-weight:800;margin:0 0 6px;">New Course Available!</h1>
      <p style="color:rgba(255,255,255,.8);font-size:14px;margin:0;">%s — we have something new for you</p>
    </div>
    <div style="padding:28px 32px;">
      <p style="color:#c8cad8;font-size:15px;margin:0 0 20px;">%s,</p>
      <div style="background:#12141c;border-radius:12px;padding:20px 22px;border:1px solid #2a2d3a;text-align:center;">
        <p style="color:#7a7d8e;font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">New Course</p>
        <p style="color:#e8e9f0;font-size:19px;font-weight:700;margin:0;">%s</p>
        %s
      </div>
      %s
      <p style="color:#4a4d5e;font-size:11px;text-align:center;margin:24px 0 0;">
        You received this because you are registered on %s.<br>
        Login to your account to explore all courses.
      </p>
    </div>
  </div>
</body>
</html>`, escapeHTML(appName), greet, escapeHTML(courseTitle), priceTag, buyBtn, escapeHTML(appName))
}

// ─── SendAccessGrantedEmail ────────────────────────────────────────────────────
// User la (admin panel varun ki Telegram /grantaccess varun) course cha access
// dilyavar - tya userच्या email var detail jaते.

func SendAccessGrantedEmail(toEmail, fullName, courseTitle, expiryInfo, appName string) error {
	if toEmail == "" {
		return fmt.Errorf("smtp: user email not set, skipping access-granted mail")
	}
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	p := primaryProvider()
	if appName == "" {
		appName = p.fromName
	}
	subject := "✅ Course Access Granted — " + courseTitle
	htmlBody := buildAccessGrantedEmail(appName, fullName, courseTitle, expiryInfo)
	return sendMail(toEmail, subject, htmlBody)
}

func buildAccessGrantedEmail(appName, fullName, courseTitle, expiryInfo string) string {
	greet := "Hi there"
	if fullName != "" {
		greet = "Hi " + escapeHTML(fullName)
	}
	expiryTag := ""
	if expiryInfo != "" {
		expiryTag = fmt.Sprintf(`<p style="color:#7a7d8e;font-size:13px;margin:10px 0 0;">%s</p>`, escapeHTML(expiryInfo))
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;margin:0;padding:40px 16px;">
  <div style="max-width:500px;margin:0 auto;background:#1a1d27;border-radius:16px;border:1px solid #2a2d3a;overflow:hidden;">
    <div style="background:linear-gradient(135deg,#16a34a,#22c55e);padding:32px 28px;text-align:center;">
      <div style="font-size:48px;margin-bottom:12px;">✅</div>
      <h1 style="color:#fff;font-size:22px;font-weight:800;margin:0 0 6px;">Access Granted!</h1>
      <p style="color:rgba(255,255,255,.85);font-size:14px;margin:0;">You can now watch this course</p>
    </div>
    <div style="padding:28px 32px;">
      <p style="color:#c8cad8;font-size:15px;margin:0 0 20px;">%s,</p>
      <div style="background:#12141c;border-radius:12px;padding:20px 22px;border:1px solid #2a2d3a;text-align:center;">
        <p style="color:#7a7d8e;font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">Course Unlocked</p>
        <p style="color:#e8e9f0;font-size:19px;font-weight:700;margin:0;">%s</p>
        %s
      </div>
      <p style="color:#4a4d5e;font-size:11px;text-align:center;margin:24px 0 0;">
        Login to your account on %s to start watching.
      </p>
    </div>
  </div>
</body>
</html>`, greet, escapeHTML(courseTitle), expiryTag, escapeHTML(appName))
}

// ─── SendRegistrationRequest ──────────────────────────────────────────────────

func SendRegistrationRequest(adminEmail, userIdentifier, userFullName string, telegramID int64) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	p := primaryProvider()
	if p.user == "" {
		return fmt.Errorf("no SMTP provider configured")
	}
	subject := "🔔 New Telegram Link Request — " + userIdentifier
	htmlBody := buildRegistrationEmail(userIdentifier, userFullName, telegramID)
	return sendMail(adminEmail, subject, htmlBody)
}

func buildRegistrationEmail(identifier, fullName string, telegramID int64) string {
	settgCmd := fmt.Sprintf("/settg %s %d", identifier, telegramID)
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="margin:0;padding:0;background:#0d0e17;font-family:'Segoe UI',Arial,sans-serif;">
  <div style="max-width:520px;margin:40px auto;background:#12141c;border-radius:16px;border:1px solid #2a2d3a;overflow:hidden;">
    <div style="background:linear-gradient(135deg,#2d6a4f,#1a9e5c);padding:28px 32px;">
      <h1 style="color:#fff;font-size:20px;font-weight:800;margin:0 0 4px;">🔔 New Link Request</h1>
      <p style="color:rgba(255,255,255,.8);font-size:13px;margin:0;">User ने Telegram account link करायला request केली</p>
    </div>
    <div style="padding:28px 32px;">
      <table style="width:100%;border-collapse:collapse;margin-bottom:24px;">
        <tr>
          <td style="color:#7a7d8e;font-size:12px;font-weight:700;text-transform:uppercase;padding:8px 0;border-bottom:1px solid #2a2d3a;">User</td>
          <td style="color:#e8e9f0;font-size:14px;font-weight:600;padding:8px 0;border-bottom:1px solid #2a2d3a;">%s</td>
        </tr>
        <tr>
          <td style="color:#7a7d8e;font-size:12px;font-weight:700;text-transform:uppercase;padding:8px 0;border-bottom:1px solid #2a2d3a;">Name</td>
          <td style="color:#e8e9f0;font-size:14px;padding:8px 0;border-bottom:1px solid #2a2d3a;">%s</td>
        </tr>
        <tr>
          <td style="color:#7a7d8e;font-size:12px;font-weight:700;text-transform:uppercase;padding:8px 0;">Telegram ID</td>
          <td style="color:#e8e9f0;font-size:14px;font-family:monospace;padding:8px 0;">%d</td>
        </tr>
      </table>
      <p style="color:#9097a5;font-size:13px;margin:0 0 12px;">Bot मध्ये हा command copy-paste करा:</p>
      <div style="background:#0d0e17;border:2px solid #1a9e5c;border-radius:10px;padding:16px 20px;font-family:monospace;font-size:16px;font-weight:700;color:#4ade80;letter-spacing:.05em;word-break:break-all;">
        %s
      </div>
      <p style="color:#4a4d5e;font-size:11px;text-align:center;margin:24px 0 0;">
        Akshayclasses Admin Panel
      </p>
    </div>
  </div>
</body>
</html>`, escapeHTML(identifier), escapeHTML(fullName), telegramID, escapeHTML(settgCmd))
}

// ─── Admin-triggered Password Change Email ────────────────────────────────────

// SendPasswordChangedEmail — admin ने password change केल्यावर user ला mail जातो.
// newPassword plain text स्वरूपात इथे येतो (एकदाच mail मध्ये दाखवायला).
func SendPasswordChangedEmail(toEmail, fullName, newPassword string) error {
	p := primaryProvider()
	subject := "Your Password Has Been Updated — " + p.fromName
	htmlBody := buildPasswordChangedEmail(p.fromName, fullName, newPassword)
	return sendMail(toEmail, subject, htmlBody)
}

func buildPasswordChangedEmail(appName, fullName, newPassword string) string {
	displayName := fullName
	if displayName == "" {
		displayName = "User"
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;margin:0;padding:40px 16px;">
  <div style="max-width:460px;margin:0 auto;background:#1a1d27;border-radius:16px;padding:40px 32px;border:1px solid #2a2d3a;">
    <div style="text-align:center;margin-bottom:28px;">
      <div style="width:60px;height:60px;background:#f59e0b;border-radius:16px;display:inline-flex;align-items:center;justify-content:center;margin-bottom:14px;">
        <span style="font-size:28px;">🔑</span>
      </div>
      <h1 style="color:#e8e9f0;font-size:20px;font-weight:700;margin:0 0 6px;">Password Updated</h1>
      <p style="color:#7a7d8e;font-size:13px;margin:0;">%s</p>
    </div>
    <p style="color:#b0b3c1;font-size:14px;line-height:1.7;margin:0 0 20px;">
      Hi <strong style="color:#e8e9f0;">%s</strong>,<br><br>
      Your account password has been updated by the admin.
      Here is your new password:
    </p>
    <div style="text-align:center;margin:20px 0;">
      <div style="display:inline-block;font-size:22px;font-weight:800;color:#f59e0b;font-family:'Courier New',monospace;background:#12141c;padding:16px 28px;border-radius:12px;border:2px solid #2a2d3a;letter-spacing:3px;">%s</div>
    </div>
    <div style="background:#12141c;border-radius:10px;padding:14px 18px;margin-top:16px;">
      <p style="color:#7a7d8e;font-size:12px;margin:0;line-height:1.7;">
        💡 You can sign in using this password or via <strong style="color:#6c63ff;">OTP on your email</strong>.<br>
        <strong style="color:#ff5c5c;">If you did not request this change, contact your admin immediately.</strong>
      </p>
    </div>
  </div>
</body>
</html>`, appName, escapeHTML(displayName), escapeHTML(newPassword))
}
