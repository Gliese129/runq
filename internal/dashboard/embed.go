//go:build dashboard

package dashboard

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embeddedStaticFS embed.FS

func embeddedDashboardFS() (fs.FS, bool) {
	sub, err := fs.Sub(embeddedStaticFS, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
