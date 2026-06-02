package dashboard

import (
	"embed"
	"io/fs"
)

//go:embed dist/*
var embeddedStaticFS embed.FS

func StaticFS() fs.FS {
	sub, err := fs.Sub(embeddedStaticFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
