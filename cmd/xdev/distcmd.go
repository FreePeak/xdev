package main

import (
	"fmt"
	"os"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/dist"
)

// runDist dispatches the distribution subcommands (M14 #64): `xdev update`,
// `xdev setup`, `xdev bench`. Each owns its own flags, so main() only routes
// the mode name here.
func runDist(sub string, args []string, version string) int {
	return dist.Run(sub, args, version, distOpenProvider, os.Stdout, os.Stderr)
}

// distOpenProvider builds the provider `xdev bench` measures through the same
// path a real run uses — resolveModel (@role/model precedence) → models.yml →
// credential chain — so the numbers describe the provider a session gets.
func distOpenProvider(ref string) (dist.BenchProvider, string, error) {
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		return nil, "", fmt.Errorf("load config: %w", err)
	}
	modelRef, _, err := resolveModel(ref, cfg, lastSettings())
	if err != nil {
		return nil, "", err
	}
	provName, modelName, err := config.ParseModelRef(modelRef)
	if err != nil {
		return nil, "", err
	}
	pc, ok := cfg.Providers[provName]
	if !ok {
		return nil, "", fmt.Errorf("unknown provider %q (have: %v)", provName, providerKeys(cfg))
	}
	prov, err := buildProvider(provName, pc, modelName, cfg)
	if err != nil {
		return nil, "", err
	}
	return prov, modelName, nil
}
