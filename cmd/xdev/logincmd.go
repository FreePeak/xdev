package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/oauth"
)

// OAuthFlow is one provider's browser-login configuration (models.yml).
type OAuthFlow struct {
	AuthorizeURL string   `yaml:"authorizeUrl"`
	TokenURL     string   `yaml:"tokenUrl"`
	ClientID     string   `yaml:"clientId"`
	Scopes       []string `yaml:"scopes,omitempty"`
	RedirectPort int      `yaml:"redirectPort,omitempty"`
}

// flowFor resolves a provider's OAuth configuration, falling back to the
// two well-known public clients (Claude Pro/Max, Codex) whose endpoints are
// stable and documented. A provider with no oauth block and no well-known
// shape has no browser login — an API-key provider is not an error, it is
// simply not OAuth.
func flowFor(provider string, cfg *config.Config) (oauth.Flow, bool, error) {
	var pc *config.ProviderConfig
	if cfg != nil {
		pc = cfg.Providers[provider]
	}
	// Known public clients first: their endpoints do not come from config.
	switch provider {
	case "claude":
		return oauth.Flow{
			AuthorizeURL: "https://claude.ai/oauth/authorize",
			TokenURL:     "https://console.anthropic.com/v1/oauth/token",
			ClientID:     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
			Scopes:       []string{"org:create_api_key", "user:profile", "user:inference"},
			RedirectPort: 54545,
		}, true, nil
	case "codex":
		return oauth.Flow{
			AuthorizeURL: "https://auth.openai.com/oauth/authorize",
			TokenURL:     "https://auth.openai.com/oauth/token",
			ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
			Scopes:       []string{"openid", "profile", "email", "offline_access"},
			RedirectPort: 1455,
		}, true, nil
	}
	if pc == nil || pc.OAuth == nil {
		return oauth.Flow{}, false, fmt.Errorf("provider %q has no oauth configuration in models.yml", provider)
	}
	o := pc.OAuth
	return oauth.Flow{
		AuthorizeURL: config.Resolve(o.AuthorizeURL),
		TokenURL:     config.Resolve(o.TokenURL),
		ClientID:     config.Resolve(o.ClientID),
		Scopes:       o.Scopes,
		RedirectPort: o.RedirectPort,
	}, true, nil
}

// refreshFunc returns the refresh hook for one provider: a closure over its
// oauth flow, so the credential chain can renew an expired token. nil when
// the provider has no oauth block (an expired api_key is not refreshable).
func refreshFunc(provider string, cfg *config.Config) func(config.StoredCredential) (config.StoredCredential, error) {
	return func(c config.StoredCredential) (config.StoredCredential, error) {
		flow, has, err := flowFor(provider, cfg)
		if err != nil {
			return config.StoredCredential{}, err
		}
		if !has {
			return config.StoredCredential{}, fmt.Errorf("provider %q has no oauth configuration", provider)
		}
		tok, err := flow.Refresh(context.Background(), c.RefreshToken)
		if err != nil {
			return config.StoredCredential{}, err
		}
		return config.StoredCredential{
			Kind:         "oauth",
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    tok.ExpiresAtMillis(time.Now()),
			Email:        c.Email,
		}, nil
	}
}

// runLogin drives the browser flow for one provider and stores the result.
func runLogin(provider string, cfg *config.Config) int {
	flow, has, err := flowFor(provider, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return 2
	}
	if !has {
		fmt.Fprintf(os.Stderr, "xdev: %s is not an OAuth provider (use its apiKey instead)\n", provider)
		return 2
	}
	fmt.Fprintf(os.Stderr, "opening browser for %s login (Ctrl+C to cancel)…\n", provider)
	res, err := oauth.Run(context.Background(), flow, openBrowser)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return 1
	}
	if res.Account != "" {
		fmt.Fprintf(os.Stderr, "logged in as %s\n", res.Account)
	}
	if err := config.SaveCredential(provider, config.StoredCredential{
		Kind:         "oauth",
		AccessToken:  res.Tokens.AccessToken,
		RefreshToken: res.Tokens.RefreshToken,
		ExpiresAt:    res.Tokens.ExpiresAtMillis(time.Now()),
		Email:        res.Account,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return 1
	}
	fmt.Printf("logged in to %s\n", provider)
	return 0
}

// runLogout removes one provider's stored credential.
func runLogout(provider string) int {
	// An unknown name is a usage error, not a success: deleting a
	// credential that never existed reported "logged out of <typo>".
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return 2
	}
	if _, ok := cfg.Providers[provider]; !ok {
		fmt.Fprintf(os.Stderr, "xdev: unknown provider %q (configured: %s)\n", provider, strings.Join(providerKeys(cfg), " "))
		return 2
	}
	if err := config.DeleteCredential(provider); err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return 1
	}
	fmt.Printf("logged out of %s\n", provider)
	return 0
}

// openBrowser opens the authorize URL with the platform opener, falling
// back to printing the URL when no opener exists.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch {
	case commandExists("xdg-open"):
		cmd = exec.Command("xdg-open", u)
	case commandExists("open"):
		cmd = exec.Command("open", u)
	default:
		fmt.Fprintln(os.Stderr, "open this URL to log in:")
		fmt.Println(u)
		return nil
	}
	return cmd.Start()
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

var _ = strings.TrimSpace
