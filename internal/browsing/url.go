package browsing

import (
	"errors"
	"net/url"
	"strings"
)

// CheckURLCredential rejects a selected model credential in the starting URL,
// including bounded percent-encoding layers, before it reaches a browser/model.
func CheckURLCredential(raw, credential string) error {
	protected := errors.New("starting URL contains protected model credentials or excessive percent encoding")
	if strings.Contains(raw, credential) {
		return protected
	}
	target, err := url.Parse(raw)
	if err != nil {
		return errors.New("starting URL is invalid")
	}
	for _, component := range []struct {
		value string
		query bool
	}{{value: target.EscapedPath()}, {value: target.RawQuery, query: true}, {value: target.EscapedFragment()}} {
		value := component.value
		for round := 0; round < 8; round++ {
			if strings.Contains(value, credential) {
				return protected
			}
			decoded := unescapeURLComponent(value, component.query)
			if decoded == value {
				break
			}
			value = decoded
		}
		if strings.Contains(value, credential) || unescapeURLComponent(value, component.query) != value {
			return protected
		}
	}
	return nil
}

func unescapeURLComponent(value string, query bool) string {
	// A valid encoded percent can become literal text at the next layer. Keep
	// those literals while decoding other escapes; they must not hide a key.
	var escaped strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '%' && (i+2 >= len(value) || !urlHex(value[i+1]) || !urlHex(value[i+2])) {
			escaped.WriteString("%25")
		} else {
			escaped.WriteByte(value[i])
		}
	}
	var decoded string
	if query {
		decoded, _ = url.QueryUnescape(escaped.String())
	} else {
		decoded, _ = url.PathUnescape(escaped.String())
	}
	return decoded
}

func urlHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}
