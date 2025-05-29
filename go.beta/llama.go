// go.beta/llama.go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	llamaCppAPIURL = "https://api.github.com/repos/ggerganov/llama.cpp/releases"
)

// GHAsset represents an asset in a GitHub release.
type GHAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

// GHRelease represents a GitHub release.
type GHRelease struct {
	TagName    string    `json:"tag_name"`
	Name       string    `json:"name"` // Release title
	Assets     []GHAsset `json:"assets"`
	Prerelease bool      `json:"prerelease"`
}

// LlamaReleaseInfo holds processed information for display and selection.
type LlamaReleaseInfo struct {
	TagName     string
	ReleaseName string
	Assets      []GHAsset // Filtered assets (e.g., binaries)
}

// formatBytes is a helper to format file sizes.
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func fetchLatestLlamaCppReleaseInfo() (*LlamaReleaseInfo, error) {
	appLogger.Println("[Llama] Fetching latest release info from:", llamaCppAPIURL)
	client := http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", llamaCppAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	// GitHub API recommends setting a User-Agent
	req.Header.Set("User-Agent", "go-downloader-app/1.0")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch releases: status %s", resp.Status)
	}

	var releases []GHRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("failed to decode release list: %w", err)
	}

	if len(releases) == 0 {
		return nil, fmt.Errorf("no releases found for ggerganov/llama.cpp")
	}

	// Find the latest non-prerelease, or the very latest if all are prereleases or none are.
	var latestRelease GHRelease
	foundNonPrerelease := false
	for _, r := range releases {
		if !r.Prerelease {
			latestRelease = r
			foundNonPrerelease = true
			break
		}
	}
	if !foundNonPrerelease && len(releases) > 0 { // All are prereleases or only one release which is a prerelease
		latestRelease = releases[0] // Fallback to the first one (usually latest published)
		appLogger.Printf("[Llama] Using latest release '%s' which is a prerelease, as no stable releases were found higher in the list.", latestRelease.TagName)
	} else if !foundNonPrerelease { // Should not happen if len(releases) > 0
		return nil, fmt.Errorf("no suitable release found (logic error)")
	}

	appLogger.Printf("[Llama] Found latest release: Tag='%s', Name='%s'", latestRelease.TagName, latestRelease.Name)

	// Filter assets - typically users want binaries, not source code zips for this utility
	var filteredAssets []GHAsset
	for _, asset := range latestRelease.Assets {
		// Common binary types or archives often containing binaries
		// Exclude source code explicitly if names match typical patterns.
		nameLower := strings.ToLower(asset.Name)
		if strings.HasPrefix(nameLower, "source code") || strings.HasSuffix(nameLower, ".tar.gz") || strings.HasSuffix(nameLower, ".zip") {
			// A more specific check for typical binary archives vs source archives
			if strings.Contains(nameLower, "source") || (strings.HasSuffix(nameLower, ".zip") && !strings.Contains(nameLower, "bin") && !strings.Contains(nameLower, "cuda") && !strings.Contains(nameLower, "macos") && !strings.Contains(nameLower, "ubuntu") && !strings.Contains(nameLower, "win")) {
				appLogger.Printf("[Llama] Skipping asset '%s' as it appears to be source code.", asset.Name)
				continue
			}
		}
		// If it's an archive and doesn't scream "source code", include it.
		// Or if it's some other content type that might be a direct binary (though less common for releases)
		filteredAssets = append(filteredAssets, asset)
	}

	if len(filteredAssets) == 0 && len(latestRelease.Assets) > 0 {
		appLogger.Printf("[Llama] No assets remained after filtering for binaries for tag '%s'. Falling back to showing all assets.", latestRelease.TagName)
		filteredAssets = latestRelease.Assets // Fallback to all assets if filter yields nothing
	}

	// Sort assets by name for consistent display
	sort.Slice(filteredAssets, func(i, j int) bool {
		return filteredAssets[i].Name < filteredAssets[j].Name
	})

	return &LlamaReleaseInfo{
		TagName:     latestRelease.TagName,
		ReleaseName: latestRelease.Name,
		Assets:      filteredAssets,
	}, nil
}

// HandleGetLlama is called when -getllama flag is used.
// It returns the DownloadItem for the selected file, the tag name for directory creation, and an error.
func HandleGetLlama() (DownloadItem, string, error) {
	releaseInfo, err := fetchLatestLlamaCppReleaseInfo()
	if err != nil {
		return DownloadItem{}, "", fmt.Errorf("could not fetch llama.cpp release info: %w", err)
	}

	if len(releaseInfo.Assets) == 0 {
		fmt.Fprintf(os.Stderr, "[INFO] No downloadable assets found for the latest release '%s' (%s).\n", releaseInfo.TagName, releaseInfo.ReleaseName)
		return DownloadItem{}, releaseInfo.TagName, nil // No error, but nothing to download
	}

	fmt.Fprintf(os.Stderr, "[INFO] Latest llama.cpp release: %s (%s)\n", releaseInfo.ReleaseName, releaseInfo.TagName)
	fmt.Fprintln(os.Stderr, "Available files for download:")
	for i, asset := range releaseInfo.Assets {
		fmt.Fprintf(os.Stderr, "%d: %s (%s)\n", i+1, asset.Name, formatBytes(asset.Size))
	}
	fmt.Fprint(os.Stderr, "Enter the number of the file to download (or 0 to cancel): ")

	reader := bufio.NewReader(os.Stdin)
	inputStr, readErr := reader.ReadString('\n')
	if readErr != nil {
		return DownloadItem{}, releaseInfo.TagName, fmt.Errorf("failed to read selection: %w", readErr)
	}
	inputStr = strings.TrimSpace(inputStr)
	selectedIndex, convErr := strconv.Atoi(inputStr)

	if convErr != nil || selectedIndex < 0 || selectedIndex > len(releaseInfo.Assets) {
		fmt.Fprintln(os.Stderr, "[INFO] Invalid selection or cancelled. No file will be downloaded.")
		return DownloadItem{}, releaseInfo.TagName, nil // No error, but no valid selection
	}

	if selectedIndex == 0 {
		fmt.Fprintln(os.Stderr, "[INFO] Download cancelled by user.")
		return DownloadItem{}, releaseInfo.TagName, nil
	}

	selectedAsset := releaseInfo.Assets[selectedIndex-1]
	appLogger.Printf("[Llama] User selected file: %s", selectedAsset.Name)
	fmt.Fprintf(os.Stderr, "[INFO] Selected for download: %s\n", selectedAsset.Name)

	return DownloadItem{
		URL:               selectedAsset.BrowserDownloadURL,
		PreferredFilename: selectedAsset.Name,
	}, releaseInfo.TagName, nil
}
