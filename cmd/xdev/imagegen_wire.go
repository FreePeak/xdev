package main

import (
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/imagegen"
)

// generate_image wiring (M15 #69), kept out of print.go so the registration
// there stays a single line.
//
// imageGenCreds resolves an image provider's key through the SAME chain the
// chat providers use — models.yml provider block (when one exists for the
// name) → stored /login credential → environment — so a user who logged in
// once for chat does not have to configure images separately. A provider with
// no models.yml block still resolves through the chain's env rung
// (OPENAI_API_KEY / GEMINI_API_KEY), which is the name the engine's error
// message tells the user to export.
func imageGenCreds() imagegen.CredentialLookup {
	return func(provider string) (key, source string, err error) {
		models, err := config.LoadModelsLayered()
		if err != nil {
			// An unreadable models.yml must not take image generation down
			// with it: the credential chain's login/env rungs remain valid.
			models = &config.Config{Providers: map[string]*config.ProviderConfig{}}
		}
		store, err := config.LoadCredentials()
		if err != nil {
			store = config.CredentialStore{}
		}
		resolved, err := config.ResolveCredential(config.CredentialRequest{
			Provider:    provider,
			ProviderCfg: models.Providers[provider],
			Store:       store,
		})
		if err != nil {
			return "", "", err
		}
		return resolved.Value, resolved.Source, nil
	}
}
