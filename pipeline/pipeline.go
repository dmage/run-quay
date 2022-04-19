package pipeline

import "embed"

// FS holds a reference to all files inside the "kustomize" directory. Files
// inside this dir are divided into multiple overlays, each in one directory.
// These overlays are applied on top of a default overlay called "base" so a
// directory with this name must exist.
//go:embed kustomize
var FS embed.FS
