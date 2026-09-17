package dist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Release channels (issue #64): stable = the newest published release;
// canary = the newest published release including pre-releases, which is
// how a tag like v0.2.0-canary.1 is discoverable before it is promoted.
const (
	ChannelStable = "stable"
	ChannelCanary = "canary"
)

// DefaultReleaseRepo is the GitHub repository releases are published to.
const DefaultReleaseRepo = "FreePeak/xdev"

// Bounds: a release asset is a ~15 MB binary and a manifest is a few
// hundred bytes, so these caps are pure runaway protection.
const (
	maxAssetBytes    = 256 << 20
	maxManifestBytes = 4 << 20
	maxReleaseBody   = 4 << 20
)

// Client resolves releases and downloads assets from the GitHub API.
// Fields (not constants) so tests point it at an httptest server and a
// corporate mirror can override the API root.
type Client struct {
	HTTP    *http.Client
	APIBase string // default https://api.github.com
	Repo    string // default DefaultReleaseRepo
	Token   string // optional: raises the 60 req/h anonymous limit
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimSuffix(c.APIBase, "/")
	}
	return "https://api.github.com"
}

func (c *Client) repo() string {
	if c.Repo != "" {
		return c.Repo
	}
	return DefaultReleaseRepo
}

// Asset is one downloadable release artifact.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"` // the API asset URL (Accept: octet-stream)
	Size int64  `json:"size"`
}

// Release is the subset of the GitHub release payload the updater reads.
type Release struct {
	Tag        string  `json:"tag_name"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Latest resolves the newest release for a channel. A channel with no
// published release returns (nil, nil): "nothing to update to" is not an
// error on a fresh repository.
func (c *Client) Latest(ctx context.Context, channel string) (*Release, error) {
	switch channel {
	case ChannelStable:
		body, err := c.get(ctx, c.apiBase()+"/repos/"+c.repo()+"/releases/latest", maxReleaseBody)
		if err != nil {
			if errIsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		var rel Release
		if err := json.Unmarshal(body, &rel); err != nil {
			return nil, fmt.Errorf("releases/latest: decode: %w", err)
		}
		if rel.Draft || rel.Prerelease {
			// GitHub's /latest already excludes both; a mirror that does
			// not gets the safe answer instead of an unverified upgrade.
			return nil, nil
		}
		return &rel, nil
	case ChannelCanary:
		body, err := c.get(ctx, c.apiBase()+"/repos/"+c.repo()+"/releases?per_page=30", maxReleaseBody)
		if err != nil {
			return nil, err
		}
		var rels []Release
		if err := json.Unmarshal(body, &rels); err != nil {
			return nil, fmt.Errorf("releases: decode: %w", err)
		}
		for i := range rels {
			if rels[i].Draft {
				continue // drafts are not published, therefore not installable
			}
			return &rels[i], nil
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown channel %q (want %s|%s)", channel, ChannelStable, ChannelCanary)
	}
}

// AssetByName finds an asset by exact name (case-insensitive). The empty
// string means "no such asset".
func (r *Release) AssetByName(names ...string) *Asset {
	for _, want := range names {
		for i := range r.Assets {
			if strings.EqualFold(r.Assets[i].Name, want) {
				return &r.Assets[i]
			}
		}
	}
	return nil
}

// AssetNames lists the asset names, for an error message that tells the
// user what the release actually carries.
func (r *Release) AssetNames() []string {
	out := make([]string, 0, len(r.Assets))
	for i := range r.Assets {
		out = append(out, r.Assets[i].Name)
	}
	return out
}

// Download streams an asset into w, bounded by maxAssetBytes.
func (c *Client) Download(ctx context.Context, a Asset, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("download %s: %s: %s", a.Name, resp.Status, strings.TrimSpace(string(body)))
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxAssetBytes))
	if err != nil {
		return fmt.Errorf("download %s: %w", a.Name, err)
	}
	if a.Size > 0 && n != a.Size {
		return fmt.Errorf("download %s: got %d bytes, release lists %d", a.Name, n, a.Size)
	}
	return nil
}

// SumsFor fetches a checksum manifest from the release and returns the
// expected SHA-256 for assetName. platform is the running machine's
// "<goos>_<goarch>", used to find a per-platform manifest. A release without
// a manifest is an error: an unverifiable binary is never installed.
func (c *Client) SumsFor(ctx context.Context, rel *Release, assetName, platform string) (string, error) {
	candidates := []string{"SHA256SUMS", "SHA256SUMS.txt", "sha256sums"}
	// Per-platform manifests are the shape M8's release job uploads today;
	// a merged SHA256SUMS is preferred when a release carries both.
	candidates = append(candidates, "SHA256SUMS_"+platform+".txt")
	manifest := rel.AssetByName(candidates...)
	if manifest == nil {
		return "", fmt.Errorf("release %s has no SHA256SUMS manifest (assets: %s); refusing to install an unverifiable binary",
			rel.Tag, strings.Join(rel.AssetNames(), ", "))
	}
	var buf strings.Builder
	if err := c.Download(ctx, *manifest, &buf); err != nil {
		return "", err
	}
	if int64(buf.Len()) > maxManifestBytes {
		return "", fmt.Errorf("manifest %s: larger than %d bytes", manifest.Name, maxManifestBytes)
	}
	sum, ok := ParseSums(buf.String())[assetName]
	if !ok {
		return "", fmt.Errorf("manifest %s has no entry for %s", manifest.Name, assetName)
	}
	return sum, nil
}

// ParseSums reads a sha256sums manifest ("<hex>  <name>", "*<name>" for
// binary mode; the name may carry a directory prefix).
func ParseSums(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 {
			continue // a malformed line contributes nothing
		}
		name := strings.TrimPrefix(strings.TrimSpace(strings.Join(fields[1:], " ")), "*")
		out[filepath.Base(name)] = sum
	}
	return out
}

// hashFile returns the lowercase hex SHA-256 of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: response larger than %d bytes", url, limit)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{Status: resp.StatusCode, URL: url, Body: strings.TrimSpace(string(body))}
	}
	return body, nil
}

type httpError struct {
	Status int
	URL    string
	Body   string
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("%s: %d %s", e.URL, e.Status, http.StatusText(e.Status))
	if e.Body != "" {
		msg += ": " + firstLine(e.Body)
	}
	return msg
}

func errIsNotFound(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// errIsAuth reports a credential rejection: a stale GITHUB_TOKEN in the
// environment must not turn a working anonymous update into a 401.
func errIsAuth(err error) bool {
	var he *httpError
	return errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
