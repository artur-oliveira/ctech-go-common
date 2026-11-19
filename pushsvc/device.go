package pushsvc

import "strings"

// DeviceLabel is a coarse, human label of a browser ("Chrome no Android") derived from its User-Agent. It is shown on
// the device list and never identifies a person.
func DeviceLabel(userAgent string) string {
	ua := strings.ToLower(userAgent)
	browser := "Navegador"
	switch {
	case strings.Contains(ua, "edg/"):
		browser = "Edge"
	case strings.Contains(ua, "samsungbrowser"):
		browser = "Samsung Internet"
	case strings.Contains(ua, "firefox") || strings.Contains(ua, "fxios"):
		browser = "Firefox"
	case strings.Contains(ua, "chrome") || strings.Contains(ua, "crios"):
		browser = "Chrome"
	case strings.Contains(ua, "safari"):
		browser = "Safari"
	}
	system := ""
	switch {
	case strings.Contains(ua, "android"):
		system = "Android"
	case strings.Contains(ua, "iphone") || strings.Contains(ua, "ipad"):
		system = "iOS"
	case strings.Contains(ua, "windows"):
		system = "Windows"
	case strings.Contains(ua, "mac os"):
		system = "macOS"
	case strings.Contains(ua, "linux"):
		system = "Linux"
	}
	if system == "" {
		return browser
	}
	return browser + " no " + system
}
