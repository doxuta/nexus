package notify

import (
	"fmt"
	"html"
	"strings"
)

// RenderResetMail renders the (subject, text, html) body for a password-reset email. This is
// the ONE sanctioned token-bearing render path in this package: renderAlert (template.go)
// deliberately forbids interpolating any secret/token by invariant (spec §1 / notify.go:14),
// so a reset link — which IS a bearer credential by definition — cannot go through it. Callers
// (services.AuthService.RequestPasswordReset) must build resetURL themselves
// (RESET_URL_BASE + "?token=" + rawToken) and pass it in whole; this function never mints or
// knows about the raw token, it only renders around the URL it is given.
//
// The URL is HTML-escaped in the HTML body (defense-in-depth against a malformed base URL
// injecting markup) and used verbatim in the plain-text body. Nothing else here is
// sensitive: no hash, no password, no other secret is ever interpolated.
func RenderResetMail(resetURL string) (subject, text, htmlBody string) {
	const (
		subj     = "Đặt lại mật khẩu tài khoản Nexus"
		headline = "Chúng tôi vừa nhận được yêu cầu đặt lại mật khẩu cho tài khoản của bạn"
		expiry   = "Đường dẫn này chỉ có hiệu lực trong một khoảng thời gian ngắn và chỉ dùng được một lần."
		ignore   = "Nếu bạn KHÔNG yêu cầu đặt lại mật khẩu, hãy bỏ qua email này — mật khẩu của bạn sẽ không thay đổi."
	)

	text = fmt.Sprintf(
		"Xin chào,\n\n%s.\n\nNhấp vào đường dẫn sau để đặt mật khẩu mới:\n%s\n\n%s\n\n%s\n\nĐây là email tự động từ hệ thống Nexus. Vui lòng không trả lời email này.",
		headline, resetURL, expiry, ignore,
	)

	var b strings.Builder
	b.WriteString("<p>Xin chào,</p>")
	b.WriteString("<p>" + html.EscapeString(headline) + ".</p>")
	b.WriteString(`<p><a href="` + html.EscapeString(resetURL) + `">Đặt lại mật khẩu</a></p>`)
	b.WriteString("<p>" + html.EscapeString(expiry) + "</p>")
	b.WriteString("<p>" + html.EscapeString(ignore) + "</p>")
	b.WriteString("<hr><p style=\"color:#888;font-size:12px\">Đây là email tự động từ hệ thống Nexus. Vui lòng không trả lời email này.</p>")

	return subj, text, b.String()
}
