package workflowlink

import (
	"net/url"
	"regexp"
	"strings"
)

// RedactRemote returns a git remote URL safe to send to the server (R34):
// user info (user:token@), the query string and the fragment are removed
// from URLs with a scheme ("https://u:tok@host/r.git?token=x" becomes
// "https://host/r.git"). scp-style remotes (git@host:o/r.git) and local
// paths carry no secret and are returned unchanged. A URL that does not
// parse, or a scheme-less string that still looks like it carries
// credentials, is replaced by "" rather than risk sending a secret.
//
// util.RedactURL strips user info only; it keeps the query string, which
// can hold a token, so it is not enough here.
func RedactRemote(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		// scp-style (user@host:path) or a local path. Anything with a query
		// marker, a password-looking "user:pw@", or whitespace is not a
		// remote gtb wrote; drop it.
		if strings.ContainsAny(raw, "?# \t") {
			return ""
		}
		if at := strings.Index(raw, "@"); at >= 0 && strings.Contains(raw[:at], ":") {
			return ""
		}
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// urlInTextRe matches scheme://... URLs inside free-form text, up to
// whitespace or a quote.
var urlInTextRe = regexp.MustCompile("[A-Za-z][A-Za-z0-9+.-]*://[^\\s'\"<>`]+")

// RedactText applies RedactRemote to every URL in s. It is used for the
// error text (sling/unsling output, git errors) a Result carries to the
// server. A URL that cannot be cleaned is replaced by "<redacted-url>".
func RedactText(s string) string {
	return urlInTextRe.ReplaceAllStringFunc(s, func(m string) string {
		if r := RedactRemote(m); r != "" {
			return r
		}
		return "<redacted-url>"
	})
}
