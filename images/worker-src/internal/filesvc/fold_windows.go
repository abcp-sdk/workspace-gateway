//go:build windows

package filesvc

import "strings"

// trimDevicePrefix drops the extended-length device prefix Windows APIs (and
// EvalSymlinks) may return: \\?\C:\... and \\.\C:\...
func trimDevicePrefix(p string) string {
	if strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return p[4:]
	}
	return p
}
