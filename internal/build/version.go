package build

// Version is set at build time via:
//
//	-ldflags "-X nimbus/internal/build.Version=vX.Y.Z"
//
// Defaults to "dev" for local/unversioned builds.
var Version = "dev"
