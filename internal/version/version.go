package version

// Version is set at build time via:
//
//	-ldflags="-X github.com/compscidr/sair/internal/version.Version=v1.2.3"
var Version = "dev"
