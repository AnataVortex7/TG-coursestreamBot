// internal/email/brevo.go
// Brevo SMTP email sender.
// Reads config from env vars (see fsb.sample.env for full list).
package email

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"os"
	"time"
)

// ─── Config (loaded once at startup) ─────────────────────────────────────────

var (
	smtpHost  string
	smtpPort  string
	smtpUser  string
	smtpPass  string
	fromEmail string
	fromName  string
)

func init() {
	smtpHost  = envOr("BREVO_SMTP_HOST", "smtp-relay.brevo.com")
	smtpPort  = envOr("BREVO_SMTP_PORT", "587")
	smtpUser  = os.Getenv("BREVO_SMTP_USER") // your Brevo login email
	smtpPass  = os.Getenv("BREVO_SMTP_PASS") // Brevo SMTP key (not account password)
	fromEmail = envOr("BREVO_FROM_EMAIL", smtpUser)
	fromName  = envOr("BREVO_FROM_NAME", "CourseStream")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ─── Health check ─────────────────────────────────────────────────────────────

// IsServiceUp dials Brevo's SMTP server AND verifies credentials.
// Returns true only when auth succeeds — a plain TCP check is not enough
// because the port can be open even when the key is wrong or unset.
func IsServiceUp() bool {
	if smtpUser == "" || smtpPass == "" {
		return false
	}

	rawConn, err := net.DialTimeout("tcp", smtpHost+":"+smtpPort, 5*time.Second)
	if err != nil {
		return false
	}

	client, err := smtp.NewClient(rawConn, smtpHost)
	if err != nil {
		return false
	}
	defer client.Close()

	tlsCfg := &tls.Config{ServerName: smtpHost, MinVersion: tls.VersionTLS12}
	if err = client.StartTLS(tlsCfg); err != nil {
		return false
	}

	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	if err = client.Auth(auth); err != nil {
		// Log so Koyeb logs show the real reason
		// Check: Koyeb dashboard → your app → Logs tab
		log.Printf("[Brevo] SMTP auth failed: %v", err)
		return false
	}

	return true
}

// ─── Send OTP ─────────────────────────────────────────────────────────────────

// SendOTP sends a 4-digit OTP to toEmail via Brevo SMTP (STARTTLS on port 587).
func SendOTP(toEmail, otpCode string) error {
	if smtpUser == "" || smtpPass == "" {
		return fmt.Errorf("brevo: BREVO_SMTP_USER or BREVO_SMTP_PASS not set")
	}

	subject := "Your Login OTP — " + fromName
	htmlBody := buildOTPEmail(fromName, otpCode)

	// Build raw MIME message
	msg := []byte(
		"From: " + fromName + " <" + fromEmail + ">\r\n" +
			"To: " + toEmail + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n" +
			"\r\n" +
			htmlBody,
	)

	addr := smtpHost + ":" + smtpPort

	// Dial with timeout
	rawConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("brevo: dial failed: %w", err)
	}

	client, err := smtp.NewClient(rawConn, smtpHost)
	if err != nil {
		return fmt.Errorf("brevo: smtp client: %w", err)
	}
	defer client.Close()

	// Upgrade to TLS (STARTTLS)
	tlsCfg := &tls.Config{ServerName: smtpHost, MinVersion: tls.VersionTLS12}
	if err = client.StartTLS(tlsCfg); err != nil {
		return fmt.Errorf("brevo: starttls: %w", err)
	}

	// Authenticate
	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("brevo: auth: %w", err)
	}

	// Envelope
	if err = client.Mail(fromEmail); err != nil {
		return fmt.Errorf("brevo: MAIL FROM: %w", err)
	}
	if err = client.Rcpt(toEmail); err != nil {
		return fmt.Errorf("brevo: RCPT TO: %w", err)
	}

	// Body
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("brevo: DATA: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return fmt.Errorf("brevo: write body: %w", err)
	}
	return w.Close()
}

// SendRegOTP sends a 4-digit OTP for email verification during registration.
func SendRegOTP(toEmail, fullName, otpCode string) error {
	if smtpUser == "" || smtpPass == "" {
		return fmt.Errorf("brevo: BREVO_SMTP_USER or BREVO_SMTP_PASS not set")
	}

	subject := "Verify your email — " + fromName
	htmlBody := buildRegOTPEmail(fromName, fullName, otpCode)

	msg := []byte(
		"From: " + fromName + " <" + fromEmail + ">\r\n" +
			"To: " + toEmail + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n" +
			"\r\n" +
			htmlBody,
	)

	addr := smtpHost + ":" + smtpPort
	rawConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("brevo: dial failed: %w", err)
	}
	client, err := smtp.NewClient(rawConn, smtpHost)
	if err != nil {
		return fmt.Errorf("brevo: smtp client: %w", err)
	}
	defer client.Close()

	tlsCfg := &tls.Config{ServerName: smtpHost, MinVersion: tls.VersionTLS12}
	if err = client.StartTLS(tlsCfg); err != nil {
		return fmt.Errorf("brevo: starttls: %w", err)
	}
	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("brevo: auth: %w", err)
	}
	if err = client.Mail(fromEmail); err != nil {
		return fmt.Errorf("brevo: MAIL FROM: %w", err)
	}
	if err = client.Rcpt(toEmail); err != nil {
		return fmt.Errorf("brevo: RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("brevo: DATA: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return fmt.Errorf("brevo: write body: %w", err)
	}
	return w.Close()
}

// ─── Email template (registration) ───────────────────────────────────────────

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

    <!-- Header -->
    <div style="text-align:center;margin-bottom:32px;">
      <div style="width:60px;height:60px;background:#6c63ff;border-radius:16px;display:inline-flex;align-items:center;justify-content:center;margin-bottom:16px;">
        <span style="font-size:28px;">🎓</span>
      </div>
      <h1 style="color:#e8e9f0;font-size:22px;font-weight:700;margin:0 0 6px;">Login OTP</h1>
      <p style="color:#7a7d8e;font-size:14px;margin:0;">%s</p>
    </div>

    <!-- OTP Box -->
    <div style="text-align:center;margin:28px 0;">
      <p style="color:#7a7d8e;font-size:14px;margin-bottom:14px;">Your one-time password</p>
      <div style="display:inline-block;letter-spacing:14px;font-size:44px;font-weight:800;color:#6c63ff;font-family:'Courier New',monospace;background:#12141c;padding:20px 28px;border-radius:14px;border:2px solid #2a2d3a;">%s</div>
      <p style="color:#7a7d8e;font-size:13px;margin-top:14px;">⏱ Valid for <strong style="color:#e8e9f0;">10 minutes</strong> · Max 3 attempts</p>
    </div>

    <!-- Footer note -->
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

// ═══════════════════════════════════════════════════════════════════════════════
// PURCHASE NOTIFICATION — admin la gift card details sah email
// ═══════════════════════════════════════════════════════════════════════════════

// PurchaseInfo — purchase email sathi lagnarya sarv details
type PurchaseInfo struct {
	// User
	UserFullName string
	UserMobile   string
	UserEmail    string
	Username     string

	// Course
	CourseTitle string
	CoursePrice string
	CourseID    string

	// Gift Card
	CardNumber string
	CardPIN    string
	Message    string
}

// SendPurchaseNotification — admin la purchase details cha email pathavto
func SendPurchaseNotification(adminEmail string, info PurchaseInfo) error {
	if smtpUser == "" || smtpPass == "" {
		return fmt.Errorf("brevo: SMTP credentials not configured")
	}
	if adminEmail == "" {
		return fmt.Errorf("brevo: ADMIN_EMAIL not set")
	}

	subject := fmt.Sprintf("🎁 New Purchase Request — %s", info.CourseTitle)
	htmlBody := buildPurchaseEmail(info)

	msg := []byte(
		"From: " + fromName + " <" + fromEmail + ">\r\n" +
			"To: " + adminEmail + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n" +
			"\r\n" +
			htmlBody,
	)

	addr := smtpHost + ":" + smtpPort
	rawConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("brevo: dial failed: %w", err)
	}
	client, err := smtp.NewClient(rawConn, smtpHost)
	if err != nil {
		return fmt.Errorf("brevo: smtp client: %w", err)
	}
	defer client.Close()

	tlsCfg := &tls.Config{ServerName: smtpHost, MinVersion: tls.VersionTLS12}
	if err = client.StartTLS(tlsCfg); err != nil {
		return fmt.Errorf("brevo: starttls: %w", err)
	}
	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("brevo: auth: %w", err)
	}
	if err = client.Mail(fromEmail); err != nil {
		return fmt.Errorf("brevo: MAIL FROM: %w", err)
	}
	if err = client.Rcpt(adminEmail); err != nil {
		return fmt.Errorf("brevo: RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("brevo: DATA: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return fmt.Errorf("brevo: write body: %w", err)
	}
	return w.Close()
}

// buildPurchaseEmail — purchase notification cha HTML email template
func buildPurchaseEmail(info PurchaseInfo) string {
	// Grant access command — mobile kiva email use karto
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

    <!-- Header -->
    <div style="background:#6c63ff;padding:24px 28px;text-align:center;">
      <div style="font-size:36px;margin-bottom:8px;">🎁</div>
      <h1 style="color:#fff;font-size:20px;font-weight:700;margin:0 0 4px;">New Purchase Request</h1>
      <p style="color:rgba(255,255,255,.8);font-size:14px;margin:0;">Gift card payment received</p>
    </div>

    <div style="padding:24px 28px;">

      <!-- User Info -->
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

      <!-- Course Info -->
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

      <!-- Gift Card -->
      <h3 style="color:#a5b4fc;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">🎁 Gift Card Details</h3>
      <table style="width:100%%;border-collapse:collapse;background:#12141c;border-radius:10px;overflow:hidden;margin-bottom:20px;">
        <tr>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#7a7d8e;font-size:13px;white-space:nowrap;">Card Number</td>
          <td style="padding:10px 16px;border-bottom:1px solid #2a2d3a;color:#e8e9f0;font-size:15px;font-weight:700;font-family:monospace;letter-spacing:2px;">%s</td>
        </tr>
        <tr>
          <td style="padding:10px 16px;%s;color:#7a7d8e;font-size:13px;white-space:nowrap;">PIN</td>
          <td style="padding:10px 16px;%s;color:#e8e9f0;font-size:15px;font-weight:700;font-family:monospace;letter-spacing:4px;">%s</td>
        </tr>
        %s
      </table>

      <!-- Grant Access Command -->
      <h3 style="color:#4ade80;font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;margin:0 0 10px;">✅ Grant Access Command</h3>
      <div style="background:#0d1f12;border:1.5px solid rgba(34,197,94,.35);border-radius:10px;padding:16px 18px;">
        <p style="color:#7a7d8e;font-size:11px;margin:0 0 8px;">Bot madhe ha command paste kara:</p>
        <code style="color:#4ade80;font-size:14px;font-weight:700;font-family:'Courier New',monospace;word-break:break-all;">%s</code>
      </div>

      <p style="color:#7a7d8e;font-size:11px;margin:16px 0 0;text-align:center;">
        Gift card verify karin access activate kara · CourseStream Admin Panel
      </p>
    </div>
  </div>
</body>
</html>`,
		// User info
		info.UserFullName,
		orDash(info.UserMobile),
		orDash(info.UserEmail),
		info.Username,
		// Course info
		info.CourseTitle,
		orDash(info.CoursePrice),
		info.CourseID,
		// Gift card
		info.CardNumber,
		borderBottom("1px solid #2a2d3a"), borderBottom("1px solid #2a2d3a"), info.CardPIN,
		msgRow,
		// Grant command
		grantCmd,
	)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func borderBottom(style string) string {
	return "border-bottom:" + style
}
