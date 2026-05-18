// internal/email/brevo.go
// Multi-SMTP fallback email sender.
// Primary SMTP uses existing BREVO_* env vars.
// Secondary and Tertiary use BREVO_*_2 and BREVO_*_3 env vars.
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
	// Primary — existing BREVO_* variables (unchanged)
	primary := smtpConfig{
		host:      envOr("BREVO_SMTP_HOST", "smtp-relay.brevo.com"),
		port:      envOr("BREVO_SMTP_PORT", "587"),
		user:      os.Getenv("BREVO_SMTP_USER"),
		pass:      os.Getenv("BREVO_SMTP_PASS"),
		fromEmail: envOr("BREVO_FROM_EMAIL", os.Getenv("BREVO_SMTP_USER")),
		fromName:  envOr("BREVO_FROM_NAME", "CourseStream"),
	}
	if primary.user != "" && primary.pass != "" {
		smtpProviders = append(smtpProviders, primary)
	}

	// Secondary — BREVO_*_2 variables
	secondary := smtpConfig{
		host:      os.Getenv("BREVO_SMTP_HOST_2"),
		port:      envOr("BREVO_SMTP_PORT_2", "587"),
		user:      os.Getenv("BREVO_SMTP_USER_2"),
		pass:      os.Getenv("BREVO_SMTP_PASS_2"),
		fromEmail: envOr("BREVO_FROM_EMAIL_2", os.Getenv("BREVO_SMTP_USER_2")),
		fromName:  envOr("BREVO_FROM_NAME", "CourseStream"),
	}
	if secondary.host != "" && secondary.user != "" && secondary.pass != "" {
		smtpProviders = append(smtpProviders, secondary)
	}

	// Tertiary — BREVO_*_3 variables
	tertiary := smtpConfig{
		host:      os.Getenv("BREVO_SMTP_HOST_3"),
		port:      envOr("BREVO_SMTP_PORT_3", "587"),
		user:      os.Getenv("BREVO_SMTP_USER_3"),
		pass:      os.Getenv("BREVO_SMTP_PASS_3"),
		fromEmail: envOr("BREVO_FROM_EMAIL_3", os.Getenv("BREVO_SMTP_USER_3")),
		fromName:  envOr("BREVO_FROM_NAME", "CourseStream"),
	}
	if tertiary.host != "" && tertiary.user != "" && tertiary.pass != "" {
		smtpProviders = append(smtpProviders, tertiary)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ─── Health check ─────────────────────────────────────────────────────────────

// IsServiceUp — at least ek provider up ahe ka te check karto
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

// ─── Core send — fallback logic ───────────────────────────────────────────────

func sendMail(toEmail string, msg []byte) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	var lastErr error
	for i, p := range smtpProviders {
		err := sendViaProvider(p, toEmail, msg)
		if err == nil {
			if i > 0 {
				log.Printf("[SMTP] Sent via fallback provider #%d (%s)", i+1, p.host)
			}
			return nil
		}
		log.Printf("[SMTP] Provider #%d (%s) failed: %v", i+1, p.host, err)
		lastErr = err
	}
	return fmt.Errorf("smtp: all providers failed, last error: %w", lastErr)
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
	return smtpConfig{fromName: "CourseStream"}
}

// ─── Send OTP ─────────────────────────────────────────────────────────────────

func SendOTP(toEmail, otpCode string) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	p := primaryProvider()
	subject := "Your Login OTP — " + p.fromName
	htmlBody := buildOTPEmail(p.fromName, otpCode)
	msg := buildMsg(p, toEmail, subject, htmlBody)
	return sendMail(toEmail, msg)
}

// ─── Send Registration OTP ────────────────────────────────────────────────────

func SendRegOTP(toEmail, fullName, otpCode string) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	p := primaryProvider()
	subject := "Verify your email — " + p.fromName
	htmlBody := buildRegOTPEmail(p.fromName, fullName, otpCode)
	msg := buildMsg(p, toEmail, subject, htmlBody)
	return sendMail(toEmail, msg)
}

// ─── Send Purchase Notification ───────────────────────────────────────────────

func SendPurchaseNotification(adminEmail string, info PurchaseInfo) error {
	if len(smtpProviders) == 0 {
		return fmt.Errorf("smtp: no providers configured")
	}
	if adminEmail == "" {
		return fmt.Errorf("smtp: ADMIN_EMAIL not set")
	}
	p := primaryProvider()
	subject := fmt.Sprintf("🎁 New Purchase Request — %s", info.CourseTitle)
	htmlBody := buildPurchaseEmail(info)
	msg := buildMsg(p, adminEmail, subject, htmlBody)
	return sendMail(adminEmail, msg)
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
        Gift card verify karin access activate kara · CourseStream Admin Panel
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
