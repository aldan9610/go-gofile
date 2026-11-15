// website_token.go
//
// Reproduction of gofile.io's client-side X-Website-Token generation.
//
// Since 2026 gofile rejects API content requests that only carry an account
// token ("error-notPremium") unless they also carry a website token computed
// by the browser as:
//
//	sha256("{userAgent}::{language}::{accountToken}::{window}::{salt}")
//
// where window = floor(unix_time / 14400) is a rotating 4-hour bucket and
// salt is a secret embedded in gofile.io/dist/js/wt.obf.js. The server
// recomputes and validates the token, so the User-Agent and X-BL (language)
// headers of the request must match the values hashed into the token.
package gofile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// WTWindowSeconds is the length of the rotating time bucket used by gofile.
const WTWindowSeconds = 14400 // 4 hours

// WebsiteToken computes the X-Website-Token for the given inputs.
//
// windowOffset shifts the time bucket by whole windows; retry a request with
// windowOffset = -1 when gofile answers "error-notPremium" right after a
// bucket boundary.
func WebsiteToken(accountToken, userAgent, language, salt string, windowOffset int, now time.Time) string {
	window := int(now.Unix()/WTWindowSeconds) + windowOffset
	raw := fmt.Sprintf("%s::%s::%s::%d::%s", userAgent, language, accountToken, window, salt)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// websiteToken computes the token with the client's own settings.
func (c *Client) websiteToken(windowOffset int) string {
	return WebsiteToken(c.token, c.userAgent, c.language, c.salt, windowOffset, c.now())
}
