package version

import "github.com/hashicorp/packer-plugin-sdk/version"

var (
	// Version is the main version number that is being run at the moment.
	Version = "0.1.0"

	// VersionPrerelease is a pre-release marker for the version. If this is ""
	// (empty string) then it means that it is a final release. Otherwise, this
	// is a pre-release such as "dev" (in development), "beta", "rc1", etc.
	VersionPrerelease = "dev"

	// VersionMetadata is metadata further describing the build type.
	VersionMetadata = ""

	// PluginVersion is used by the plugin set to allow Packer to recognize
	// what version this plugin is.
	PluginVersion = version.NewPluginVersion(Version, VersionPrerelease, VersionMetadata)
)
