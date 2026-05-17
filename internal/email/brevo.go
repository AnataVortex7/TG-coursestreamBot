// internal/email/brevo.go
// Brevo SMTP email sender.
// Reads config from env vars (see fsb.sample.env for full list).
package email

import (
	"crypto/tls"
	"fmt"
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

// IsServiceUp does a quick TCP dial to Brevo's SMTP port (no email sent).
// Returns true if reachable within 5 seconds.
func IsServiceUp() bool {
	conn, err := net.DialTimeout("tcp", smtpHost+":"+smtpPort, 5*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
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

// ─── Email template ───────────────────────────────────────────────────────────

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
