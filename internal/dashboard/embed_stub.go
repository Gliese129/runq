//go:build !dashboard

package dashboard

import "io/fs"

func embeddedDashboardFS() (fs.FS, bool) {
	return nil, false
}
