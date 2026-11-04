package xgin

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// BearerTokenExtractor extracts one RFC 6750 bearer credential from Authorization.
// It does not verify signatures, expiry, sessions or permissions.
type BearerTokenExtractor struct{}

var _ ITokenExtractor = BearerTokenExtractor{}

// ExtractToken rejects missing, repeated and malformed Authorization credentials.
func (BearerTokenExtractor) ExtractToken(c *gin.Context) (string, bool) {
	if c == nil || c.Request == nil {
		return "", false
	}
	values := c.Request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, credential, separated := strings.Cut(values[0], " ")
	if !separated || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	// RFC 6750 allows one or more ASCII spaces, not arbitrary Unicode whitespace.
	token := strings.TrimLeft(credential, " ")
	padding := false
	for i, char := range token {
		if char == '=' {
			if i == 0 {
				return "", false
			}
			padding = true
			continue
		}
		if padding {
			return "", false
		}
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-._~+/", char) {
			continue
		}
		return "", false
	}
	return token, token != ""
}
