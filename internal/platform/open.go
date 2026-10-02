package platform

import "runtime"

// OpenBrowser opens url in the host's default browser.
func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return HiddenCommand("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return HiddenCommand("open", url).Start()
	default:
		return HiddenCommand("xdg-open", url).Start()
	}
}
