package notify

import (
	"fmt"
	"log/slog"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// EarlyAccessNotification is the payload sent on each new early-access sign-up.
type EarlyAccessNotification struct {
	Email       string
	FeatureName string
	MerchantID  string // empty if unauthenticated
	RequestedAt time.Time
	TotalCount  int64 // total signups for this feature so far
}

// SendEarlyAccessAlert fires an internal e-mail to the team when a new early-access
// request is received. It reads configuration from environment variables so no
// credentials are hard-coded.
//
// Required env vars:
//
//	SMTP_HOST          e.g. "smtp.gmail.com"
//	SMTP_PORT          e.g. "587"
//	SMTP_USERNAME      sender address / auth username
//	SMTP_PASSWORD      SMTP password or app-password
//	NOTIFY_TO_EMAIL    comma-separated recipient(s), e.g. "team@kaughtman.com"
//
// If any required variable is missing the function logs a warning and returns
// without error — notification failure must never break the user-facing API.
func SendEarlyAccessAlert(n EarlyAccessNotification) {
	host := os.Getenv("SMTP_HOST")
	port := os.Getenv("SMTP_PORT")
	username := os.Getenv("SMTP_USERNAME")
	password := os.Getenv("SMTP_PASSWORD")
	toRaw := os.Getenv("NOTIFY_TO_EMAIL")

	if host == "" || port == "" || username == "" || password == "" || toRaw == "" {
		slog.Warn("notify: SMTP not configured — skipping early-access alert email",
			"feature", n.FeatureName, "email", n.Email)
		return
	}

	recipients := parseRecipients(toRaw)
	subject := fmt.Sprintf("[Kaughtman] New Early Access Request — %s", humanFeatureName(n.FeatureName))
	body := buildBody(n)

	msg := buildMIME(username, recipients, subject, body)
	addr := host + ":" + port

	auth := smtp.PlainAuth("", username, password, host)
	if err := smtp.SendMail(addr, auth, username, recipients, []byte(msg)); err != nil {
		slog.Error("notify: failed to send early-access alert email",
			"error", err, "feature", n.FeatureName, "to", toRaw)
		return
	}

	slog.Info("notify: early-access alert sent",
		"feature", n.FeatureName, "email", n.Email, "total_count", n.TotalCount)
}

// buildBody assembles a plain-text email body.
func buildBody(n EarlyAccessNotification) string {
	merchantLine := "Unauthenticated visitor"
	if n.MerchantID != "" {
		merchantLine = "Merchant ID: " + n.MerchantID
	}

	return fmt.Sprintf(`New Early Access Request on Kaughtman

Feature     : %s
Email       : %s
Merchant    : %s
Requested At: %s IST
Total Signups for this feature: %d

--
Kaughtman Automated Alerts
`,
		humanFeatureName(n.FeatureName),
		n.Email,
		merchantLine,
		n.RequestedAt.In(ist()).Format("02 Jan 2006 15:04:05"),
		n.TotalCount,
	)
}

// buildMIME assembles a minimal RFC 2822 message.
func buildMIME(from string, to []string, subject, body string) string {
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		from,
		strings.Join(to, ", "),
		subject,
		body,
	)
}

// parseRecipients splits a comma-separated recipient string.
func parseRecipients(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// humanFeatureName converts an underscore feature key to a readable label.
func humanFeatureName(key string) string {
	replacer := strings.NewReplacer("_", " ")
	words := strings.Fields(replacer.Replace(key))
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// ist returns the Asia/Kolkata location, falling back to +05:30 fixed offset.
func ist() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+30*60)
	}
	return loc
}
