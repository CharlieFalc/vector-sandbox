module transform-registry

go 1.22

require github.com/google/uuid v1.6.0

// oapi-codegen is used only as a CLI code-generation tool (make generate).
// It is NOT a runtime dependency of the binary — only the generated types
// in gen/types.gen.go are compiled in.  Add it here via tools.go so
// `go mod tidy` keeps it in go.sum and `make install-tools` uses a
// reproducible version.
require github.com/oapi-codegen/oapi-codegen/v2 v2.4.1
