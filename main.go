package main

import (
	"fmt"
	"os"

	"github.com/hashicorp/packer-plugin-sdk/plugin"

	treadmarkPP "github.com/mcowser-p/packer-plugin-treadmark/post-processor/treadmark"
	treadmarkProv "github.com/mcowser-p/packer-plugin-treadmark/provisioner/treadmark"
	"github.com/mcowser-p/packer-plugin-treadmark/version"
)

func main() {
	pps := plugin.NewSet()
	pps.RegisterProvisioner(plugin.DEFAULT_NAME, new(treadmarkProv.Provisioner))
	pps.RegisterPostProcessor(plugin.DEFAULT_NAME, new(treadmarkPP.PostProcessor))
	pps.SetVersion(version.PluginVersion)
	err := pps.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
