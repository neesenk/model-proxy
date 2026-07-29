package web

import (
	"embed"
	"io/fs"
)

// embeddedAssets is immutable application data. Callers receive it through an
// fs.FS interface so the package does not expose a mutable package global.
//
//go:embed assets/*
var embeddedAssets embed.FS

func defaultAssets() fs.FS {
	return embeddedAssets
}
