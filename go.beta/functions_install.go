// go.beta/functions_install.go
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/mod/semver" // For comparing versions if tags were semantic
)

const (
	llamaCppOwner         = "ggerganov"
	llamaCppRepo          = "llama.cpp"
	installedAppDirPrefix = "./" // Install apps in subdirectories of the current directory
	versionFileName       = ".release_tag"
)

// getAppPath constructs the path for an installed application.
func getAppPath(appName string) string {
	return filepath.Join(installedAppDirPrefix, appName)
}

// readInstalledVersion reads the version tag from the app's directory.
func readInstalledVersion(appName string) (string, error) {
	versionFilePath := filepath.Join(getAppPath(appName), versionFileName)
	tagBytes, err := os.ReadFile(versionFilePath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(tagBytes)), nil
}

// writeInstalledVersion writes the version tag to the app's directory.
func writeInstalledVersion(appName string, tagName string) error {
	appPath := getAppPath(appName)
	if err := os.MkdirAll(appPath, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", appPath, err)
	}
	versionFilePath := filepath.Join(appPath, versionFileName)
	return os.WriteFile(versionFilePath, []byte(tagName), 0644)
}

// Regex for parsing CUDA version from asset names like "cuda-11.7"
var cudaVersionRegex = regexp.MustCompile(`cuda-(\d{1,2})\.(\d{1,2})`)

// parseCudaVersionFromAssetName extracts CUDA major and minor versions from an asset name.
func parseCudaVersionFromAssetName(assetNameLower string) (major, minor int, found bool) {
	matches := cudaVersionRegex.FindStringSubmatch(assetNameLower)
	if len(matches) == 3 { // matches[0] is full string, matches[1] is major, matches[2] is minor
		majorConv, errMaj := strconv.Atoi(matches[1])
		minorConv, errMin := strconv.Atoi(matches[2])
		if errMaj == nil && errMin == nil {
			return majorConv, minorConv, true
		}
	}
	return 0, 0, false
}

// selectLlamaAsset selects the appropriate asset from a release based on appName, OS, and Arch.
func selectLlamaAsset(assets []GHAsset, appName string, releaseTag string) *GHAsset {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	var bestAsset *GHAsset
	bestScore := -1

	appLogger.Printf("[Install] Selecting asset for appName: %s, OS: %s, Arch: %s, Release: %s", appName, goos, goarch, releaseTag)

	for i := range assets {
		asset := assets[i]
		assetNameLower := strings.ToLower(asset.Name)
		currentScore := 0

		appLogger.Printf("[Install] Considering asset: %s", asset.Name)

		// Based on the provided examples, all relevant assets are .zip files.
		if !strings.HasSuffix(assetNameLower, ".zip") {
			appLogger.Printf("[Install] Skipping asset '%s': not a .zip archive (which is expected for these artifacts).", asset.Name)
			continue
		}

		// Skip source code archives
		if strings.Contains(assetNameLower, "source") || assetNameLower == "source_code.zip" || assetNameLower == "source_code.tar.gz" {
			appLogger.Printf("[Install] Skipping asset '%s': appears to be source code.", asset.Name)
			continue
		}

		// --- OS Matching ---
		assetOs := ""
		if strings.Contains(assetNameLower, "win") {
			assetOs = "windows"
			if goos == "windows" {
				currentScore += 30
			}
		} else if strings.Contains(assetNameLower, "ubuntu") || strings.Contains(assetNameLower, "linux") {
			assetOs = "linux"
			if goos == "linux" {
				currentScore += 30
			}
		} else if strings.Contains(assetNameLower, "macos") || strings.Contains(assetNameLower, "apple") {
			assetOs = "darwin"
			if goos == "darwin" {
				currentScore += 30
			}
		}

		// --- Arch Matching ---
		assetArch := ""
		// Prioritize "x64" over "amd64" if both were possible, but check for either.
		if strings.Contains(assetNameLower, "x64") { // Common for Windows/Linux
			assetArch = "amd64"
			if goarch == "amd64" {
				currentScore += 20
			}
		} else if strings.Contains(assetNameLower, "amd64") { // Less common in these specific names but good to check
			assetArch = "amd64"
			if goarch == "amd64" {
				currentScore += 20
			}
		} else if strings.Contains(assetNameLower, "arm64") {
			assetArch = "arm64"
			if goarch == "arm64" {
				currentScore += 20
			}
		}
		// Note: "arm" alone could be ambiguous (e.g. 32-bit arm). The examples use "arm64".

		// --- AppName Specific Scoring & Filtering ---
		cudaMajor, cudaMinor, cudaFound := parseCudaVersionFromAssetName(assetNameLower)

		initialScore := currentScore // Save score from OS/Arch match

		switch appName {
		case "llama": // Generic: Prefer CPU for current platform, then general, then Vulkan. CUDA is less preferred.
			if goos != assetOs || goarch != assetArch {
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama': OS/Arch mismatch (Sys: %s/%s, Asset: %s/%s)", asset.Name, goos, goarch, assetOs, assetArch)
				continue // Must match current platform for generic "llama"
			}
			if strings.Contains(assetNameLower, "cpu") {
				currentScore += 50 // Strong preference for CPU version
			} else if strings.Contains(assetNameLower, "vulkan") {
				currentScore += 15 // Vulkan is an acceptable accelerator
			} else if cudaFound {
				currentScore += 5 // CUDA is less preferred for a generic "llama" request
			} else {
				// If no specific accelerator tag (cpu, cuda, vulkan) but OS/arch match,
				// assume it's a general build (often CPU-based by default for llama.cpp simple builds)
				currentScore += 25 // Good score for a general platform-matching binary
			}

		case "llama-win-cuda":
			if goos != "windows" || assetOs != "windows" { // Must be Windows
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama-win-cuda': requires Windows OS.", asset.Name)
				continue
			}
			if goarch != "amd64" || assetArch != "amd64" { // Must be amd64 for common CUDA builds
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama-win-cuda': requires x64 architecture.", asset.Name)
				continue
			}
			if !cudaFound {
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama-win-cuda': no CUDA version indication found in name.", asset.Name)
				continue // Needs CUDA
			}
			currentScore += 50 // Base score for being a CUDA Windows x64 asset
			if strings.Contains(assetNameLower, "cudart") {
				currentScore += 30 // `cudart` bundle is highly preferred
			}
			// Add score for CUDA version (newer is better)
			currentScore += cudaMajor*10 + cudaMinor // e.g., 11.7 -> 117, 12.4 -> 124

		case "llama-mac-arm":
			if goos != "darwin" || assetOs != "darwin" { // Must be macOS
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama-mac-arm': requires macOS.", asset.Name)
				continue
			}
			if goarch != "arm64" || assetArch != "arm64" { // Must be arm64
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama-mac-arm': requires arm64 architecture.", asset.Name)
				continue
			}
			currentScore += 50 // Base score for being macOS arm64
			// Metal is often implied for macos-arm64 builds from llama.cpp
			if strings.Contains(assetNameLower, "metal") {
				currentScore += 10
			}

		case "llama-linux-cuda":
			if goos != "linux" || assetOs != "linux" { // Must be Linux
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama-linux-cuda': requires Linux OS.", asset.Name)
				continue
			}
			if goarch != assetArch { // Architecture must also match
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama-linux-cuda': Arch mismatch (Sys: %s, Asset: %s)", asset.Name, goarch, assetArch)
				continue
			}
			if !cudaFound {
				appLogger.Printf("[Install] Skipping asset '%s' for 'llama-linux-cuda': no CUDA version indication found.", asset.Name)
				continue // Needs CUDA
			}
			currentScore += 50                              // Base score for being a CUDA Linux asset for the correct arch
			if strings.Contains(assetNameLower, "cudart") { // If cudart bundles for Linux appear
				currentScore += 30
			}
			// Add score for CUDA version
			currentScore += cudaMajor*10 + cudaMinor

		default:
			appLogger.Printf("[Install] Unknown appName: '%s'. Cannot select asset automatically using this appName.", appName)
			return nil
		}

		// If score hasn't increased beyond initial OS/Arch match, it means appName specific criteria were not met positively.
		// This check helps to ensure that we don't pick an OS/Arch matching asset if it doesn't fit appName's feature request (e.g., -cuda but no cuda tags).
		if currentScore == initialScore && (strings.Contains(appName, "cuda") || strings.Contains(appName, "cpu") || strings.Contains(appName, "arm")) {
			// Re-evaluate if it should be skipped. If initialScore is already 0 because of OS/Arch mismatch, this is fine.
			// If initialScore > 0, but no appName specific features were matched (e.g., looking for CUDA, but asset has no CUDA tags),
			// this asset might not be suitable. Let the zero currentScore after this block handle it if needed.
			// The `continue` statements within the switch cases are more direct for this.
			// If an asset made it through the switch without a continue, it means it's a candidate.
		}

		// Common keywords boost score slightly, could act as a tie-breaker
		if strings.Contains(assetNameLower, "bin") {
			currentScore += 2
		}

		appLogger.Printf("[Install] Asset '%s' intermediate score %d (OS: %s, Arch: %s, CUDA: %t [%d.%d])", asset.Name, currentScore, assetOs, assetArch, cudaFound, cudaMajor, cudaMinor)

		// If currentScore is 0, it means it didn't match basic requirements (like OS/Arch for "llama", or specific needs for others)
		if currentScore <= 0 { // Or some threshold if initial points were given for just being a zip.
			appLogger.Printf("[Install] Asset '%s' final score %d is too low or non-matching, skipping.", asset.Name, currentScore)
			continue
		}

		if currentScore > bestScore {
			bestScore = currentScore
			clonedAsset := asset // Make a copy to avoid issues if 'asset' is reused by range
			bestAsset = &clonedAsset
			appLogger.Printf("[Install] New best asset for '%s': '%s' (Score: %d)", appName, bestAsset.Name, bestScore)
		} else if currentScore == bestScore && bestAsset != nil {
			// Tie-breaking: could prefer shorter names, or specific keywords if absolutely necessary
			// For now, first one with best score wins if not overridden by more specific tie-breaker.
			// Example: if two assets score identically, prefer one with "cudart" if current best doesn't have it.
			if strings.Contains(assetNameLower, "cudart") && !strings.Contains(strings.ToLower(bestAsset.Name), "cudart") {
				appLogger.Printf("[Install] Tie-breaking: Preferring '%s' with 'cudart' over '%s' (Score: %d)", asset.Name, bestAsset.Name, currentScore)
				clonedAsset := asset
				bestAsset = &clonedAsset
			}
			// Another tie-breaker: If appName indicates CUDA, prefer higher CUDA version on tie.
			// This is already handled by CUDA version scoring if base scores are equal.
		}
	}

	if bestAsset != nil {
		appLogger.Printf("[Install] Final best matching asset for '%s': '%s' (Score: %d)", appName, bestAsset.Name, bestScore)
	} else {
		appLogger.Printf("[Install] No suitable asset found for '%s' in release '%s' after checking all assets.", appName, releaseTag)
	}
	return bestAsset
}

// downloadAndUnpackAsset downloads and unpacks an asset.
func downloadAndUnpackAsset(pm *ProgressManager, asset GHAsset, appName string, appPath string) error {
	// Create a ProgressWriter for this download
	// ActualFileName will be the name of the downloaded archive file.
	// downloadDir will be the appPath itself, so the archive is saved in, e.g. ./llama/asset.zip
	pw := newProgressWriter(0, asset.BrowserDownloadURL, asset.Name, asset.Size, pm)
	pm.AddInitialDownloads([]*ProgressWriter{pw}) // Add and trigger initial draw

	var downloadWG sync.WaitGroup
	downloadWG.Add(1)

	fmt.Fprintf(os.Stderr, "[INFO] Downloading %s to %s...\n", asset.Name, appPath)
	appLogger.Printf("[Install] Starting download for asset %s from %s", asset.Name, asset.BrowserDownloadURL)

	// The downloadFile function from downloader.go expects a base downloadDir
	// and the pw.ActualFileName is relative to that.
	// Here, we want to download to appPath/asset.Name
	go downloadFile(pw, &downloadWG, appPath, pm)
	downloadWG.Wait()

	if pw.ErrorMsg != "" {
		return fmt.Errorf("failed to download %s: %s", asset.Name, pw.ErrorMsg)
	}
	appLogger.Printf("[Install] Download complete: %s", filepath.Join(appPath, asset.Name))
	fmt.Fprintf(os.Stderr, "[INFO] Download complete: %s\n", asset.Name)

	// Unpack
	downloadedFilePath := filepath.Join(appPath, asset.Name)
	fmt.Fprintf(os.Stderr, "[INFO] Unpacking %s to %s...\n", asset.Name, appPath)
	appLogger.Printf("[Install] Unpacking %s to %s", downloadedFilePath, appPath)

	var unpackErr error
	if strings.HasSuffix(strings.ToLower(asset.Name), ".zip") {
		unpackErr = unzip(downloadedFilePath, appPath)
	} else if strings.HasSuffix(strings.ToLower(asset.Name), ".tar.gz") {
		unpackErr = untarGz(downloadedFilePath, appPath)
	} else if strings.HasSuffix(strings.ToLower(asset.Name), ".exe") || !strings.Contains(asset.Name, ".") { // Assume raw binary
		// For raw binaries (like server executables), it's already "unpacked".
		// We might want to ensure it has execute permissions if not on Windows.
		if runtime.GOOS != "windows" {
			if err := os.Chmod(downloadedFilePath, 0755); err != nil {
				appLogger.Printf("[Install] Warning: failed to chmod +x %s: %v", downloadedFilePath, err)
			}
		}
		appLogger.Printf("[Install] Asset %s is a raw binary, no unpacking needed.", asset.Name)
	} else {
		unpackErr = fmt.Errorf("unsupported archive format: %s", asset.Name)
	}

	if unpackErr != nil {
		return fmt.Errorf("failed to unpack %s: %w", asset.Name, unpackErr)
	}

	// Clean up downloaded archive
	if !(strings.HasSuffix(strings.ToLower(asset.Name), ".exe") || !strings.Contains(asset.Name, ".")) { // Don't remove if it was the raw binary itself
		appLogger.Printf("[Install] Removing archive %s", downloadedFilePath)
		if err := os.Remove(downloadedFilePath); err != nil {
			appLogger.Printf("[Install] Warning: failed to remove archive %s: %v", downloadedFilePath, err)
		}
	}

	fmt.Fprintf(os.Stderr, "[INFO] Unpacking complete.\n")
	return nil
}

// HandleInstallLlamaApp installs a llama.cpp application.
func HandleInstallLlamaApp(pm *ProgressManager, appName string) {
	appLogger.Printf("[Install] Attempting to install app: %s", appName)
	fmt.Fprintf(os.Stderr, "[INFO] Starting installation for %s...\n", appName)

	appPath := getAppPath(appName)
	if _, err := os.Stat(appPath); err == nil {
		fmt.Fprintf(os.Stderr, "[WARN] Application '%s' seems to be already installed at %s.\n", appName, appPath)
		fmt.Fprint(os.Stderr, "Do you want to remove the existing installation and proceed? (yes/No): ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(input)) != "yes" {
			fmt.Fprintln(os.Stderr, "[INFO] Installation aborted by user.")
			appLogger.Printf("[Install] Installation of %s aborted by user (directory exists).", appName)
			return
		}
		appLogger.Printf("[Install] User chose to remove existing directory %s and reinstall.", appPath)
		if err := os.RemoveAll(appPath); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Failed to remove existing directory %s: %v\n", appPath, err)
			appLogger.Printf("[Install] Failed to remove existing directory %s: %v", appPath, err)
			return
		}
		fmt.Fprintf(os.Stderr, "[INFO] Existing directory %s removed.\n", appPath)
	}

	fmt.Fprintln(os.Stderr, "[INFO] Fetching latest release information for llama.cpp...")
	// Use the existing fetch function. We might need to adapt it or its usage if filtering is too aggressive.
	// For now, assume fetchLatestLlamaCppReleaseInfo is in llama.go and returns sufficient assets.
	releaseInfo, err := fetchLatestLlamaCppReleaseInfo() // This function is in llama.go
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Could not fetch llama.cpp release info: %v\n", err)
		appLogger.Printf("[Install] Error fetching llama.cpp release info: %v", err)
		return
	}
	appLogger.Printf("[Install] Fetched latest release: %s (%s)", releaseInfo.ReleaseName, releaseInfo.TagName)
	fmt.Fprintf(os.Stderr, "[INFO] Latest llama.cpp release: %s (Tag: %s)\n", releaseInfo.ReleaseName, releaseInfo.TagName)

	selectedAsset := selectLlamaAsset(releaseInfo.Assets, appName, releaseInfo.TagName)
	if selectedAsset == nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Could not find a suitable asset for '%s' in release %s.\n", appName, releaseInfo.TagName)
		fmt.Fprintln(os.Stderr, "Please check the app name or available assets in the release.")
		appLogger.Printf("[Install] No suitable asset found for %s.", appName)
		return
	}
	appLogger.Printf("[Install] Selected asset for %s: %s", appName, selectedAsset.Name)
	fmt.Fprintf(os.Stderr, "[INFO] Selected asset: %s (Size: %s)\n", selectedAsset.Name, formatBytes(selectedAsset.Size))

	if err := os.MkdirAll(appPath, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to create application directory %s: %v\n", appPath, err)
		appLogger.Printf("[Install] Failed to create dir %s: %v", appPath, err)
		return
	}

	err = downloadAndUnpackAsset(pm, *selectedAsset, appName, appPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to download and unpack %s: %v\n", selectedAsset.Name, err)
		appLogger.Printf("[Install] Error in download/unpack for %s: %v", selectedAsset.Name, err)
		// Attempt to clean up failed installation directory
		os.RemoveAll(appPath)
		return
	}

	if err := writeInstalledVersion(appName, releaseInfo.TagName); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to write version information for %s: %v\n", appName, err)
		appLogger.Printf("[Install] Failed to write version for %s: %v", appName, err)
		// Installation mostly succeeded, but version tracking failed.
		return
	}

	fmt.Fprintf(os.Stderr, "[SUCCESS] %s (Version: %s) installed successfully to %s\n", appName, releaseInfo.TagName, appPath)
	appLogger.Printf("[Install] %s version %s installed to %s", appName, releaseInfo.TagName, appPath)
}

// HandleUpdateLlamaApp updates a llama.cpp application.
func HandleUpdateLlamaApp(pm *ProgressManager, appName string) {
	appLogger.Printf("[Update] Attempting to update app: %s", appName)
	fmt.Fprintf(os.Stderr, "[INFO] Checking for updates for %s...\n", appName)

	appPath := getAppPath(appName)
	if _, err := os.Stat(appPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "[ERROR] Application %s is not installed at %s. Please install it first.\n", appName, appPath)
		appLogger.Printf("[Update] App %s not found at %s for update.", appName, appPath)
		return
	}

	currentTag, err := readInstalledVersion(appName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Could not read installed version for %s: %v\n", appName, err)
		fmt.Fprintln(os.Stderr, "The application might be corrupted. Try reinstalling.")
		appLogger.Printf("[Update] Error reading installed version for %s: %v", appName, err)
		return
	}
	appLogger.Printf("[Update] Current installed version of %s: %s", appName, currentTag)
	fmt.Fprintf(os.Stderr, "[INFO] Current installed version of %s: %s\n", appName, currentTag)

	fmt.Fprintln(os.Stderr, "[INFO] Fetching latest release information for llama.cpp...")
	latestReleaseInfo, err := fetchLatestLlamaCppReleaseInfo()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Could not fetch llama.cpp release info: %v\n", err)
		appLogger.Printf("[Update] Error fetching llama.cpp release info: %v", err)
		return
	}
	latestTag := latestReleaseInfo.TagName
	appLogger.Printf("[Update] Latest available version: %s", latestTag)
	fmt.Fprintf(os.Stderr, "[INFO] Latest available version of llama.cpp: %s\n", latestTag)

	// llama.cpp tags like "b2927" are not semantic versions. Direct string comparison works if format is consistent.
	// Or, if tags were proper semver: semver.Compare("v"+latestTag, "v"+currentTag) > 0
	if latestTag == currentTag {
		fmt.Fprintf(os.Stderr, "[INFO] %s is already up to date (Version: %s).\n", appName, currentTag)
		appLogger.Printf("[Update] %s is already up to date.", appName)
		return
	}
	// Simple string comparison for build tags like "bXXXX". Assumes higher number/lexicographically greater means newer.
	if latestTag < currentTag && !(strings.HasPrefix(latestTag, "master-") && strings.HasPrefix(currentTag, "b")) { // Edge case for old "master-" tags vs new "b" tags
		// This condition means currentTag is "newer" or different format. For "bXXXX" tags, this implies current is newer.
		// However, if latest is a "b" tag and current is an old "master-" tag, we should update.
		fmt.Fprintf(os.Stderr, "[INFO] Your current version (%s) seems newer or different from the latest stable (%s). No update performed.\n", currentTag, latestTag)
		appLogger.Printf("[Update] Current version %s of %s seems newer than latest %s. No update.", currentTag, appName, latestTag)
		return
	}

	fmt.Fprintf(os.Stderr, "[INFO] New version %s available for %s. Current version is %s.\n", latestTag, appName, currentTag)
	appLogger.Printf("[Update] New version %s available for %s (current: %s).", latestTag, appName, currentTag)

	selectedAsset := selectLlamaAsset(latestReleaseInfo.Assets, appName, latestTag)
	if selectedAsset == nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Could not find a suitable asset for '%s' in release %s for update.\n", appName, latestTag)
		appLogger.Printf("[Update] No suitable asset found for %s in new release %s.", appName, latestTag)
		return
	}
	appLogger.Printf("[Update] Selected asset for update: %s", selectedAsset.Name)
	fmt.Fprintf(os.Stderr, "[INFO] Update asset: %s (Size: %s)\n", selectedAsset.Name, formatBytes(selectedAsset.Size))

	// Perform update: remove old files (except .version_tag), then download and unpack new.
	// More robust: download to temp, unpack to temp, then move.
	// Simpler: remove all, then reinstall logic.
	fmt.Fprintf(os.Stderr, "[INFO] Removing old version of %s before updating...\n", appName)
	appLogger.Printf("[Update] Removing old files in %s for update.", appPath)

	// List files, remove all except potentially logs or configs if we add them later
	dirEntries, err := os.ReadDir(appPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to read directory %s for cleanup: %v\n", appPath, err)
		appLogger.Printf("[Update] Failed to read dir %s for cleanup: %v", appPath, err)
		return
	}
	for _, entry := range dirEntries {
		// Keep the version file to avoid issues if update fails mid-way, or remove it and only write new one on full success.
		// For now, let's remove everything and rely on full success of download/unpack.
		// if entry.Name() == versionFileName { continue }
		if err := os.RemoveAll(filepath.Join(appPath, entry.Name())); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Failed to remove old file/directory %s: %v\n", entry.Name(), err)
			appLogger.Printf("[Update] Failed to remove %s: %v", filepath.Join(appPath, entry.Name()), err)
			return // Stop update if cleanup fails
		}
	}
	appLogger.Printf("[Update] Old files removed from %s.", appPath)

	err = downloadAndUnpackAsset(pm, *selectedAsset, appName, appPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to download and unpack update for %s: %v\n", appName, err)
		appLogger.Printf("[Update] Error in download/unpack for update of %s: %v", appName, err)
		fmt.Fprintln(os.Stderr, "[INFO] Update failed. The application directory might be in an inconsistent state. Consider reinstalling.")
		// Attempt to restore version file? Or leave it, as it's now a failed update.
		return
	}

	if err := writeInstalledVersion(appName, latestTag); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to write updated version information for %s: %v\n", appName, err)
		appLogger.Printf("[Update] Failed to write new version for %s: %v", appName, err)
		return
	}

	fmt.Fprintf(os.Stderr, "[SUCCESS] %s updated successfully to version %s in %s\n", appName, latestTag, appPath)
	appLogger.Printf("[Update] %s updated to %s in %s", appName, latestTag, appPath)
}

// HandleRemoveLlamaApp removes a llama.cpp application.
func HandleRemoveLlamaApp(appName string) {
	appLogger.Printf("[Remove] Attempting to remove app: %s", appName)
	fmt.Fprintf(os.Stderr, "[INFO] Attempting to remove %s...\n", appName)

	appPath := getAppPath(appName)
	if _, err := os.Stat(appPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "[INFO] Application %s is not installed at %s (or already removed).\n", appName, appPath)
		appLogger.Printf("[Remove] App %s not found at %s for removal.", appName, appPath)
		return
	}

	fmt.Fprintf(os.Stderr, "Are you sure you want to remove %s from %s? (yes/No): ", appName, appPath)
	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(input)) != "yes" {
		fmt.Fprintln(os.Stderr, "[INFO] Removal aborted by user.")
		appLogger.Printf("[Remove] Removal of %s aborted by user.", appName)
		return
	}

	if err := os.RemoveAll(appPath); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to remove %s: %v\n", appName, err)
		appLogger.Printf("[Remove] Failed to remove dir %s: %v", appPath, err)
		return
	}

	fmt.Fprintf(os.Stderr, "[SUCCESS] %s removed successfully from %s.\n", appName, appPath)
	appLogger.Printf("[Remove] %s removed from %s.", appName, appPath)
}

// --- Unarchiving functions ---

func unzip(src, dest string) error {
	appLogger.Printf("[Unzip] Unzipping %s to %s", src, dest)
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("failed to open zip %s: %w", src, err)
	}
	defer r.Close()

	// Ensure destination directory exists
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory %s: %w", dest, err)
	}

	for _, f := range r.File {
		filePath := filepath.Join(dest, f.Name)
		appLogger.Printf("[Unzip] Extracting file: %s", filePath)

		// Sanitize file path to prevent path traversal
		if !strings.HasPrefix(filePath, filepath.Clean(dest)+string(os.PathSeparator)) && dest != "." {
			return fmt.Errorf("illegal file path in zip: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(filePath, f.Mode()); err != nil {
				return fmt.Errorf("failed to create directory %s from zip: %w", filePath, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(filePath), os.ModePerm); err != nil {
			return fmt.Errorf("failed to create parent directory for %s: %w", filePath, err)
		}

		outFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return fmt.Errorf("failed to create file %s from zip: %w", filePath, err)
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return fmt.Errorf("failed to open file in zip %s: %w", f.Name, err)
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close() // Close file before checking copy error
		rc.Close()      // Close reader from zip file

		if err != nil {
			return fmt.Errorf("failed to copy content for %s from zip: %w", f.Name, err)
		}
	}
	appLogger.Printf("[Unzip] Successfully unzipped %s", src)
	return nil
}

func untarGz(src, dest string) error {
	appLogger.Printf("[UntarGz] Untarring %s to %s", src, dest)
	fileReader, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open tar.gz %s: %w", src, err)
	}
	defer fileReader.Close()

	gzReader, err := gzip.NewReader(fileReader)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader for %s: %w", src, err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	// Ensure destination directory exists
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory %s: %w", dest, err)
	}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break // End of tar archive
		}
		if err != nil {
			return fmt.Errorf("failed to read next tar header: %w", err)
		}

		targetPath := filepath.Join(dest, header.Name)
		appLogger.Printf("[UntarGz] Extracting: %s", targetPath)

		// Sanitize file path
		if !strings.HasPrefix(targetPath, filepath.Clean(dest)+string(os.PathSeparator)) && dest != "." {
			return fmt.Errorf("illegal file path in tar.gz: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("failed to create directory %s from tar.gz: %w", targetPath, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), os.ModePerm); err != nil {
				return fmt.Errorf("failed to create parent directory for %s: %w", targetPath, err)
			}
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed to create file %s from tar.gz: %w", targetPath, err)
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to copy content for %s from tar.gz: %w", targetPath, err)
			}
			outFile.Close()
		case tar.TypeSymlink:
			// Handling symlinks can be complex and platform-dependent, especially regarding security.
			// For now, we'll log and skip symlinks. Production code might need careful handling.
			appLogger.Printf("[UntarGz] Skipping symlink: %s -> %s", targetPath, header.Linkname)
			fmt.Fprintf(os.Stderr, "[WARN] Skipping symbolic link from archive: %s -> %s\n", header.Name, header.Linkname)
		default:
			appLogger.Printf("[UntarGz] Unsupported tar entry type %c for %s", header.Typeflag, header.Name)
			// Optionally, return an error here if strictness is required
			// return fmt.Errorf("unsupported tar entry type %c for %s", header.Typeflag, header.Name)
		}
	}
	appLogger.Printf("[UntarGz] Successfully untarred %s", src)
	return nil
}

// This is a placeholder for semver.Compare if we were using it for llama.cpp tags.
// Llama.cpp tags are not always semver (e.g., "b2927").
// For such tags, direct string comparison or custom logic is needed.
// The update logic uses direct string comparison for "bXXXX" tags for now.
func compareVersions(v1, v2 string) int {
	// Normalize if they are like "v1.2.3"
	if !strings.HasPrefix(v1, "v") {
		v1 = "v" + v1
	}
	if !strings.HasPrefix(v2, "v") {
		v2 = "v" + v2
	}
	if semver.IsValid(v1) && semver.IsValid(v2) {
		return semver.Compare(v1, v2)
	}
	// Fallback for non-semver tags, simple string comparison
	if v1 > v2 {
		return 1
	}
	if v1 < v2 {
		return -1
	}
	return 0
}
