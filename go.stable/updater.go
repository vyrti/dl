// go.beta/updater.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings" // For string manipulation
	"time"

	"golang.org/x/mod/semver" // For semantic version comparison
)

const (
	updaterRepoOwner = "vyrti"
	updaterRepoName  = "dl"
	// CurrentAppVersion can be set at build time using ldflags:
	// go build -ldflags="-X main.CurrentAppVersion=v0.1.0"
	// or for development builds:
	// go build -ldflags="-X main.CurrentAppVersion=DEVELOPMENT"
	// This allows checking if an update is actually newer.
	CurrentAppVersion  = "v0.1.3"      // Default if not set by ldflags
	DevelopmentVersion = "DEVELOPMENT" // Special string to indicate a development build
)

// GHAssetUpdater represents an asset in a GitHub release for the updater.
type GHAssetUpdater struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// GHReleaseUpdater represents a GitHub release for the updater.
type GHReleaseUpdater struct {
	TagName string           `json:"tag_name"`
	Name    string           `json:"name"` // Release title
	Assets  []GHAssetUpdater `json:"assets"`
}

// appLogger is expected to be a global logger instance defined elsewhere in the 'main' package
// (e.g., in main.go or downloader.go).
// Example:
// var appLogger *log.Logger

func platformArchToAssetName(goos, goarch string) (string, error) {
	// Asset names must match those produced in build.sh
	switch goos {
	case "darwin":
		switch goarch {
		case "amd64":
			return "dl.apple.intel", nil
		case "arm64":
			return "dl.apple.arm", nil
		}
	case "windows":
		switch goarch {
		case "amd64":
			return "dl.win.x64.exe", nil
		case "arm64":
			return "dl.win.arm.exe", nil
		}
	case "linux":
		switch goarch {
		case "amd64":
			return "dl.linux.x64", nil
		case "arm64":
			return "dl.linux.arm", nil
		}
	}
	return "", fmt.Errorf("unsupported platform-architecture combination for update: %s/%s", goos, goarch)
}

func fetchLatestUpdateRelease(owner, repo string) (*GHReleaseUpdater, error) {
	// ...
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	appLogger.Printf("[Updater] Fetching latest release info from: %s", apiURL)

	client := http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for GitHub API: %w", err)
	}
	req.Header.Set("User-Agent", "Go-Downloader-Updater/1.0")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API request failed with status %s for URL %s", resp.Status, apiURL)
	}

	var release GHReleaseUpdater
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to decode release JSON: %w", err)
	}
	return &release, nil
}

func findMatchingAssetForUpdate(release *GHReleaseUpdater, targetAssetName string) *GHAssetUpdater {
	// ...
	for i := range release.Assets {
		if release.Assets[i].Name == targetAssetName {
			asset := &release.Assets[i]
			appLogger.Printf("[Updater] Found matching asset: %s (Size: %d)", asset.Name, asset.Size)
			return asset
		}
	}
	appLogger.Printf("[Updater] No asset found with name: %s in release %s", targetAssetName, release.TagName)
	return nil
}

func downloadFileForUpdate(url string, destPath string, assetSize int64) error {
	// ...
	appLogger.Printf("[Updater] Downloading update from %s to %s", url, destPath)
	fmt.Fprintf(os.Stderr, "[INFO] Downloading update from %s...\n", url)

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", destPath, err)
	}
	defer out.Close()

	client := http.Client{Timeout: 30 * time.Minute}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for download: %w", err)
	}
	req.Header.Set("User-Agent", "Go-Downloader-Updater/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download request failed: status %s", resp.Status)
	}

	totalSize := resp.ContentLength
	if totalSize <= 0 && assetSize > 0 {
		totalSize = assetSize
	}
	if totalSize <= 0 {
		appLogger.Printf("[Updater] Warning: Total size for download is unknown. Progress percentage will not be shown accurately.")
	}

	var downloaded int64
	buf := make([]byte, 32*1024)
	startTime := time.Now()

	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := out.Write(buf[0:nr])
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
			downloaded += int64(nw)

			if totalSize > 0 {
				percent := float64(downloaded) * 100 / float64(totalSize)
				fmt.Fprintf(os.Stderr, "\rDownloading update: %.2f%% ", percent)
			} else {
				fmt.Fprintf(os.Stderr, "\rDownloading update: %.2f MB ", float64(downloaded)/(1024*1024))
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	fmt.Fprintln(os.Stderr)

	if err != nil {
		os.Remove(destPath)
		return fmt.Errorf("error during download stream: %w", err)
	}

	appLogger.Printf("[Updater] Downloaded %d bytes in %s", downloaded, time.Since(startTime))
	if totalSize > 0 && downloaded != totalSize {
		appLogger.Printf("[Updater] Warning: Downloaded size (%d) does not match expected size (%d). File may be incomplete or corrupted.", downloaded, totalSize)
	}
	return nil
}

// HandleUpdate performs the self-update process.
func HandleUpdate() {
	appLogger.Println("[Updater] Starting update process.")
	fmt.Fprintln(os.Stderr, "[INFO] Checking for updates...")

	currentExecPath, err := os.Executable()
	if err != nil {
		appLogger.Printf("[Updater] Error getting current executable path: %v", err)
		fmt.Fprintf(os.Stderr, "[ERROR] Could not determine application path: %v\n", err)
		os.Exit(1)
	}
	appLogger.Printf("[Updater] Current executable path: %s", currentExecPath)

	targetAssetName, err := platformArchToAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		appLogger.Printf("[Updater] %v", err)
		fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
		fmt.Fprintln(os.Stderr, "[INFO] Auto-update not supported for your system configuration.")
		os.Exit(1)
	}
	appLogger.Printf("[Updater] Target asset name for this platform (%s/%s): %s", runtime.GOOS, runtime.GOARCH, targetAssetName)

	release, err := fetchLatestUpdateRelease(updaterRepoOwner, updaterRepoName)
	if err != nil {
		appLogger.Printf("[Updater] Error fetching release info: %v", err)
		fmt.Fprintf(os.Stderr, "[ERROR] Could not fetch update information: %v\n", err)
		os.Exit(1)
	}
	appLogger.Printf("[Updater] Latest release found: %s (Tag: %s)", release.Name, release.TagName)
	appLogger.Printf("[Updater] Current app version: %s", CurrentAppVersion)

	// Version Check
	if CurrentAppVersion == DevelopmentVersion {
		appLogger.Printf("[Updater] Running development version (%s). Proceeding with update check for release %s.", DevelopmentVersion, release.TagName)
		fmt.Fprintf(os.Stderr, "[INFO] Running development version. Checking for release %s...\n", release.TagName)
	} else {
		// Normalize CurrentAppVersion for comparison (e.g., ensure 'v' prefix if it's like "1.2.3")
		currentVersionToCompare := CurrentAppVersion
		if !strings.HasPrefix(currentVersionToCompare, "v") && semver.IsValid("v"+currentVersionToCompare) {
			currentVersionToCompare = "v" + currentVersionToCompare
		}

		if !semver.IsValid(currentVersionToCompare) {
			appLogger.Printf("[Updater] Warning: Current app version string '%s' (normalized to '%s') is not a valid semantic version. Proceeding with update, but version comparison is unreliable.", CurrentAppVersion, currentVersionToCompare)
			fmt.Fprintf(os.Stderr, "[WARN] Your current application version (%s) is not standard. Attempting update from %s...\n", CurrentAppVersion, release.TagName)
		} else {
			// CurrentAppVersion is valid semver. Now check and normalize release.TagName.
			releaseVersionToCompare := release.TagName
			if !strings.HasPrefix(releaseVersionToCompare, "v") && semver.IsValid("v"+releaseVersionToCompare) {
				releaseVersionToCompare = "v" + releaseVersionToCompare
			}

			if !semver.IsValid(releaseVersionToCompare) {
				appLogger.Printf("[Updater] Error: Latest release tag '%s' (normalized to '%s') is not a valid semantic version. Cannot compare versions. Aborting update.", release.TagName, releaseVersionToCompare)
				fmt.Fprintf(os.Stderr, "[ERROR] The latest release tag (%s) is not a recognized version. Cannot perform update.\n", release.TagName)
				os.Exit(1) // Abort if the remote tag is not understandable.
			}

			// Both versions are valid semver. Compare them.
			comparisonResult := semver.Compare(releaseVersionToCompare, currentVersionToCompare)

			if comparisonResult > 0 {
				// New version is available
				appLogger.Printf("[Updater] New version %s is available (current: %s).", releaseVersionToCompare, currentVersionToCompare)
				fmt.Fprintf(os.Stderr, "[INFO] A new version %s is available. (Your current version: %s)\n", release.TagName, CurrentAppVersion)
			} else {
				// Current version is the same or newer
				message := "is the same as"
				if comparisonResult < 0 {
					message = "is newer than"
				}
				appLogger.Printf("[Updater] Current version %s %s the latest release version %s. No update needed.", currentVersionToCompare, message, releaseVersionToCompare)
				fmt.Fprintf(os.Stderr, "[INFO] Your current version (%s) %s the latest available version (%s). No update needed.\n", CurrentAppVersion, message, release.TagName)
				os.Exit(0)
			}
		}
	}

	asset := findMatchingAssetForUpdate(release, targetAssetName)
	if asset == nil {
		appLogger.Printf("[Updater] No suitable update asset found for '%s' in release %s.", targetAssetName, release.TagName)
		fmt.Fprintf(os.Stderr, "[INFO] No update found for your platform/architecture (%s) in the latest release (%s).\n", targetAssetName, release.TagName)
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr, "[INFO] Found update: %s (Version: %s, Size: %.2f MB)\n", asset.Name, release.TagName, float64(asset.Size)/(1024*1024))

	tempDownloadPath := currentExecPath + ".new"
	os.Remove(tempDownloadPath) // Clean up any old temp file

	if err := downloadFileForUpdate(asset.BrowserDownloadURL, tempDownloadPath, asset.Size); err != nil {
		appLogger.Printf("[Updater] Failed to download update: %v", err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to download update: %v\n", err)
		os.Remove(tempDownloadPath) // Clean up
		os.Exit(1)
	}

	if err := os.Chmod(tempDownloadPath, 0755); err != nil {
		appLogger.Printf("[Updater] Warning: Failed to set executable permission on %s: %v", tempDownloadPath, err)
		if runtime.GOOS != "windows" {
			fmt.Fprintf(os.Stderr, "[ERROR] Failed to make update executable: %v. Please check permissions.\n", err)
			os.Remove(tempDownloadPath)
			os.Exit(1)
		}
	}

	appLogger.Printf("[Updater] Update downloaded to %s. Attempting to replace current executable.", tempDownloadPath)
	fmt.Fprintln(os.Stderr, "[INFO] Applying update...")

	oldExecPath := currentExecPath + ".old"
	os.Remove(oldExecPath)

	if err := os.Rename(currentExecPath, oldExecPath); err != nil {
		appLogger.Printf("[Updater] Failed to rename current executable %s to %s: %v", currentExecPath, oldExecPath, err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to backup current application: %v\n", err)
		fmt.Fprintln(os.Stderr, "         Please ensure the application has write permissions to its directory, and that it's not locked.")
		os.Remove(tempDownloadPath)
		os.Exit(1)
	}
	appLogger.Printf("[Updater] Renamed %s to %s", currentExecPath, oldExecPath)

	if err := os.Rename(tempDownloadPath, currentExecPath); err != nil {
		appLogger.Printf("[Updater] Failed to rename new executable %s to %s: %v", tempDownloadPath, currentExecPath, err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to apply update: %v\n", err)
		if errRestore := os.Rename(oldExecPath, currentExecPath); errRestore != nil {
			appLogger.Printf("[Updater] CRITICAL: Failed to restore backup %s to %s: %v", oldExecPath, currentExecPath, errRestore)
			fmt.Fprintf(os.Stderr, "[CRITICAL] Failed to restore backup. Application may be in an inconsistent state. Old version at: %s\n", oldExecPath)
		} else {
			appLogger.Printf("[Updater] Restored backup %s to %s", oldExecPath, currentExecPath)
			fmt.Fprintln(os.Stderr, "[INFO] Backup restored. Update failed.")
		}
		os.Exit(1)
	}
	appLogger.Printf("[Updater] Renamed %s to %s. Update applied.", tempDownloadPath, currentExecPath)

	fmt.Fprintln(os.Stderr, "[INFO] Update successful!")
	fmt.Fprintln(os.Stderr, "[INFO] Please restart the application to use the new version.")
	appLogger.Println("[Updater] Update process completed successfully. Exiting.")
	os.Exit(0)
}
