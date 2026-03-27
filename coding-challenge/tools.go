//go:build tools

// Package tools pins CLI tool versions to go.mod so `go mod tidy` tracks
// them and `make install-tools` installs the exact same version across all
// developer machines and CI environments.
//
// These imports are never compiled into the binary (build tag "tools" is
// never set during normal builds).
package tools

import (
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
)
