//go:build !windows

package filesvc

// trimDevicePrefix: no device prefixes on unix.
func trimDevicePrefix(p string) string { return p }
