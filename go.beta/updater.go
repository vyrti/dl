// go.beta/updater.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"
)

const (
	updaterRepoOwner = "vyrti"
	updaterRepoName  = "dl"
	// CurrentAppVersion can be set at build time using ldflags:
	// go build -ldflags="-X main.CurrentAppVersion=v0.1.0"
	// This allows checking if an update is actually newer.
	// For this implementation, we'll assume an update is always performed if --update is called.
	CurrentAppVersion = "v0.1.0" // Default if not set by ldflags
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

func platformArchToAssetName(goos, goarch string) (string, error) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return "dl.x64", nil
		case "arm64":
			return "dl.arm", nil // Assuming dl.arm is for arm64
		}
	case "windows":
		switch goarch {
		case "amd64":
			return "dl.x64.exe", nil
		case "arm64":
			return "dl.arm.exe", nil
		}
	case "darwin":
		switch goarch {
		case "amd64":
			return "dl.intel.mac", nil
		case "arm64":
			return "dl.arm.mac", nil
		}
	}
	return "", fmt.Errorf("unsupported platform-architecture combination for update: %s/%s", goos, goarch)
}

func fetchLatestUpdateRelease(owner, repo string) (*GHReleaseUpdater, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	appLogger.Printf("[Updater] Fetching latest release info from: %s", apiURL)

	client := http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for GitHub API: %w", err)
	}
	req.Header.Set("User-Agent", "Go-Downloader-Updater/1.0") // It's good practice to set a User-Agent
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
	for i := range release.Assets { // Iterate by index to get a pointer to the element
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
	appLogger.Printf("[Updater] Downloading update from %s to %s", url, destPath)
	fmt.Fprintf(os.Stderr, "[INFO] Downloading update from %s...\n", url)

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", destPath, err)
	}
	defer out.Close()

	client := http.Client{Timeout: 30 * time.Minute} // Generous timeout for large downloads
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
	if totalSize <= 0 && assetSize > 0 { // Fallback to asset.Size if ContentLength is not helpful
		totalSize = assetSize
	}
	if totalSize <= 0 {
		appLogger.Printf("[Updater] Warning: Total size for download is unknown. Progress percentage will not be shown accurately.")
	}

	var downloaded int64
	buf := make([]byte, 32*1024) // 32KB buffer
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
				// If total size is unknown, show bytes downloaded
				fmt.Fprintf(os.Stderr, "\rDownloading update: %.2f MB ", float64(downloaded)/(1024*1024))
			}
		}
		if er != nil {
			if er != io.EOF { // io.EOF is expected at the end
				err = er
			}
			break
		}
	}
	fmt.Fprintln(os.Stderr) // Newline after progress updates are done

	if err != nil {
		os.Remove(destPath) // Attempt to clean up partially downloaded file
		return fmt.Errorf("error during download stream: %w", err)
	}

	appLogger.Printf("[Updater] Downloaded %d bytes in %s", downloaded, time.Since(startTime))
	if totalSize > 0 && downloaded != totalSize {
		appLogger.Printf("[Updater] Warning: Downloaded size (%d) does not match expected size (%d). File may be incomplete or corrupted.", downloaded, totalSize)
		// Consider this an error if strict size matching is required.
		// For now, it's a warning, and the update continues.
		// return fmt.Errorf("downloaded size mismatch: %d vs %d, download may be corrupt", downloaded, totalSize)
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

	// Optional: Version check (if CurrentAppVersion is set via ldflags)
	// if release.TagName == CurrentAppVersion && CurrentAppVersion != "DEVELOPMENT" {
	// 	appLogger.Printf("[Updater] Current version %s is already the latest version %s.", CurrentAppVersion, release.TagName)
	// 	fmt.Fprintf(os.Stderr, "[INFO] You are already running the latest version (%s).\n", CurrentAppVersion)
	// 	os.Exit(0)
	// }
	// fmt.Fprintf(os.Stderr, "[INFO] Latest version available: %s. Your version: %s.\n", release.TagName, CurrentAppVersion)

	asset := findMatchingAssetForUpdate(release, targetAssetName)
	if asset == nil {
		appLogger.Printf("[Updater] No suitable update asset found for '%s' in release %s.", targetAssetName, release.TagName)
		fmt.Fprintf(os.Stderr, "[INFO] No update found for your platform/architecture (%s) in the latest release (%s).\n", targetAssetName, release.TagName)
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr, "[INFO] Found update: %s (Version: %s, Size: %.2f MB)\n", asset.Name, release.TagName, float64(asset.Size)/(1024*1024))

	tempDownloadPath := currentExecPath + ".new"
	// Clean up any old temp file first
	os.Remove(tempDownloadPath)

	if err := downloadFileForUpdate(asset.BrowserDownloadURL, tempDownloadPath, asset.Size); err != nil {
		appLogger.Printf("[Updater] Failed to download update: %v", err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to download update: %v\n", err)
		os.Remove(tempDownloadPath) // Clean up
		os.Exit(1)
	}

	// Ensure the downloaded file is executable (especially for Unix-like systems)
	// On Windows, Chmod is mostly a no-op for executability but doesn't hurt.
	if err := os.Chmod(tempDownloadPath, 0755); err != nil {
		appLogger.Printf("[Updater] Warning: Failed to set executable permission on %s: %v", tempDownloadPath, err)
		// This is more critical for Linux/macOS than Windows.
		if runtime.GOOS != "windows" {
			fmt.Fprintf(os.Stderr, "[ERROR] Failed to make update executable: %v. Please check permissions for the directory containing the application.\n", err)
			os.Remove(tempDownloadPath)
			os.Exit(1)
		}
	}

	appLogger.Printf("[Updater] Update downloaded to %s. Attempting to replace current executable.", tempDownloadPath)
	fmt.Fprintln(os.Stderr, "[INFO] Applying update...")

	oldExecPath := currentExecPath + ".old"
	// Remove any pre-existing .old file to avoid issues with os.Rename
	os.Remove(oldExecPath)

	// Rename current executable to .old
	if err := os.Rename(currentExecPath, oldExecPath); err != nil {
		appLogger.Printf("[Updater] Failed to rename current executable %s to %s: %v", currentExecPath, oldExecPath, err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to backup current application: %v\n", err)
		fmt.Fprintln(os.Stderr, "         Please ensure the application has write permissions to its directory, and that it's not locked by another process.")
		os.Remove(tempDownloadPath) // Clean up downloaded file
		os.Exit(1)
	}
	appLogger.Printf("[Updater] Renamed %s to %s", currentExecPath, oldExecPath)

	// Rename new executable to current executable's path
	if err := os.Rename(tempDownloadPath, currentExecPath); err != nil {
		appLogger.Printf("[Updater] Failed to rename new executable %s to %s: %v", tempDownloadPath, currentExecPath, err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to apply update: %v\n", err)
		// Attempt to restore backup
		if errRestore := os.Rename(oldExecPath, currentExecPath); errRestore != nil {
			appLogger.Printf("[Updater] CRITICAL: Failed to restore backup %s to %s: %v", oldExecPath, currentExecPath, errRestore)
			fmt.Fprintf(os.Stderr, "[CRITICAL] Failed to restore backup. Application may be in an inconsistent state. The old version might be at: %s\n", oldExecPath)
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
