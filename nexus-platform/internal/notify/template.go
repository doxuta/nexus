package notify

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// renderTimestamp formats an event time for a Vietnamese reader. We use a fixed,
// locale-friendly layout (24h, day/month/year) in the event's own location; callers
// pass evt.At as captured at the originating event.
func renderTimestamp(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.Format("15:04 02/01/2006")
}

// alertCopy is the locale (vi) copy for one alert type: a Subject line plus a single
// human sentence describing what happened. The dispatcher wraps this with the timestamp
// and a fixed advice footer. Copy is deliberately minimal and contains NO token, hash,
// password or other secret (spec §1 / §7).
type alertCopy struct {
	subject  string
	headline string // "what happened" sentence, no trailing period
}

// viCopy maps each alert type to its Vietnamese copy. An unknown type falls back to a
// generic security-notice headline so a new event never panics or leaks raw type names
// into a subject in an alarming way.
var viCopy = map[string]alertCopy{
	AlertPasswordChanged: {
		subject:  "Mật khẩu tài khoản của bạn vừa được thay đổi",
		headline: "Mật khẩu cho tài khoản của bạn vừa được thay đổi",
	},
	AlertRoleChanged: {
		subject:  "Quyền truy cập tài khoản của bạn vừa thay đổi",
		headline: "Quyền truy cập (vai trò) trên tài khoản của bạn vừa được thay đổi",
	},
	AlertAccountDisabled: {
		subject:  "Tài khoản của bạn vừa bị vô hiệu hoá",
		headline: "Tài khoản của bạn vừa bị vô hiệu hoá",
	},
	AlertAccountDeleted: {
		subject:  "Tài khoản của bạn vừa bị xoá",
		headline: "Tài khoản của bạn vừa bị xoá khỏi hệ thống",
	},
	AlertRefreshReuse: {
		subject:  "Cảnh báo bảo mật: phát hiện hoạt động đăng nhập bất thường",
		headline: "Hệ thống phát hiện một phiên đăng nhập đã hết hạn bị sử dụng lại trên tài khoản của bạn",
	},
	AlertMFAEnabled: {
		subject:  "Xác thực hai lớp (MFA) đã được bật trên tài khoản của bạn",
		headline: "Xác thực hai lớp (MFA) vừa được bật trên tài khoản của bạn",
	},
	AlertMFADisabled: {
		subject:  "Xác thực hai lớp (MFA) đã bị tắt trên tài khoản của bạn",
		headline: "Xác thực hai lớp (MFA) vừa bị tắt trên tài khoản của bạn",
	},
}

// adviceFor returns the closing advice line for an alert type. refresh_reuse gets a
// stronger call to action (likely token theft → all sessions were revoked).
func adviceFor(alertType string) string {
	if alertType == AlertRefreshReuse {
		return "Vì lý do an toàn, hệ thống đã đăng xuất tất cả các phiên trên tài khoản này. " +
			"Nếu đây KHÔNG phải là bạn, hãy đổi mật khẩu ngay và liên hệ quản trị viên."
	}
	return "Nếu đây KHÔNG phải là hành động do bạn thực hiện, hãy đổi mật khẩu ngay " +
		"và liên hệ quản trị viên của bạn."
}

// renderAlert renders the (subject, text, html) for an event in the vi locale. The body
// is intentionally minimal: what happened + when + advice. It NEVER includes evt.Meta
// values that could be sensitive — only the fixed copy, the formatted timestamp, and a
// neutral advice footer are emitted, so no token/hash/password can leak.
func renderAlert(evt SecurityEvent) (subject, text, htmlBody string) {
	copy, ok := viCopy[evt.Type]
	if !ok {
		copy = alertCopy{
			subject:  "Thông báo bảo mật trên tài khoản của bạn",
			headline: "Một sự kiện bảo mật vừa xảy ra trên tài khoản của bạn",
		}
	}

	when := renderTimestamp(evt.At)
	advice := adviceFor(evt.Type)

	text = fmt.Sprintf(
		"Xin chào,\n\n%s vào lúc %s.\n\n%s\n\nĐây là email tự động từ hệ thống Nexus. Vui lòng không trả lời email này.",
		copy.headline, when, advice,
	)

	// HTML mirrors the text exactly; every dynamic segment is HTML-escaped so a crafted
	// timestamp/headline can never inject markup. No secret values are interpolated.
	var b strings.Builder
	b.WriteString("<p>Xin chào,</p>")
	b.WriteString("<p>" + html.EscapeString(copy.headline) + " vào lúc <strong>" + html.EscapeString(when) + "</strong>.</p>")
	b.WriteString("<p>" + html.EscapeString(advice) + "</p>")
	b.WriteString("<hr><p style=\"color:#888;font-size:12px\">Đây là email tự động từ hệ thống Nexus. Vui lòng không trả lời email này.</p>")

	return copy.subject, text, b.String()
}
