package helmplugin

import (
	"fmt"

	"github.com/jenkins-x-plugins/jx-admin/pkg/plugins"
	"github.com/jenkins-x/jx-helpers/v3/pkg/helmer"
)

// GetHelm3Binary returns the location of the helm 3 binary
func GetHelm3Binary() (string, error) {
	return plugins.GetHelmBinary(plugins.HelmVersion)
}

// NewHelm3Helmer returns a new helm 3 helmer
func NewHelm3Helmer(cwd string) (*helmer.HelmCLI, error) {
	helmBin, err := plugins.GetHelmBinary(plugins.HelmVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to find helm binary: %w", err)
	}
	return NewHelmer(helmBin, cwd), nil
}

// NewHelmer creates a new helmer from the given binary
func NewHelmer(helmBin, cwd string) *helmer.HelmCLI {
	return helmer.NewHelmCLIWithRunner(nil, helmBin, cwd, false)
}
