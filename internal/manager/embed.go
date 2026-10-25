package manager

import "embed"

// WebAssets embeds all files from the web directory.
//
//go:embed web/*
var WebAssets embed.FS
