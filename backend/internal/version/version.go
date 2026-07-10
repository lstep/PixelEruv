// Package version holds the build version, injected via ldflags at compile
// time. The Makefile sets it to the git tag (if HEAD is exactly on a tag) or
// the short commit hash. Defaults to "dev" for unbuilt or Docker-dev builds.
package version

var Version = "dev"
