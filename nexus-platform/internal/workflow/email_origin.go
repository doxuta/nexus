package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// EmailOrigin says where one outgoing mail comes from — config ids only, never rendered data
// (specs/066 D-2/D-3). runAction attaches it to ctx so an EmailSender can write the tenant's email
// log without the EmailSender signature changing.
type EmailOrigin struct {
	TenantID, Source                 string // Source "" = workflow
	WorkflowID, EntityName, RecordID string
	ActionIndex                      int
	SubjectTemplate, ActionHash      string // a.Subject; sha256(To \x00 Subject \x00 Body templates)
	UserID                           string // the account an auth mail is about
	LogID, ResendOf, CreatedBy       string // resend: pre-minted row id, the row it replaces, the actor
}

type emailOriginKey struct{}

// WithEmailOrigin returns ctx carrying o.
func WithEmailOrigin(ctx context.Context, o EmailOrigin) context.Context {
	return context.WithValue(ctx, emailOriginKey{}, o)
}

// EmailOriginFrom returns the origin runAction attached; ok=false outside a rule's email action.
func EmailOriginFrom(ctx context.Context) (EmailOrigin, bool) {
	o, ok := ctx.Value(emailOriginKey{}).(EmailOrigin)
	return o, ok
}

// maskedRecipientsMax is sys_email_log.recipient_masked's width.
const maskedRecipientsMax = 255

// MaskRecipients reduces a rendered recipient list to what the log may keep: per address the first
// rune + "***@" + domain ("l***@gmail.com"), a part without "@" becomes "***". Parts split on , and
// ;; the result is cut at 255 bytes on a rune boundary.
func MaskRecipients(to string) string {
	var out []string
	for _, p := range strings.FieldsFunc(to, func(r rune) bool { return r == ',' || r == ';' }) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		at := strings.LastIndex(p, "@")
		if at <= 0 {
			out = append(out, "***")
			continue
		}
		first, _ := utf8.DecodeRuneInString(p)
		out = append(out, string(first)+"***"+p[at:])
	}
	s := strings.Join(out, ", ")
	for len(s) > maskedRecipientsMax {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// EmailActionHash fingerprints the action's TEMPLATES (not rendered text) so a later resend can say
// "the rule changed since this was sent".
func EmailActionHash(a Action) string {
	sum := sha256.Sum256([]byte(a.To + "\x00" + a.Subject + "\x00" + a.Body))
	return hex.EncodeToString(sum[:])
}
