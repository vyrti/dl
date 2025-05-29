// go.beta/test.go
package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

// ShowSystemInfo displays system hardware information.
// Note: Gathering detailed hardware info like RAM speed, GPU model, and VRAM details
// is highly platform-dependent and often requires administrator privileges
// or parsing output from specific command-line tools.
// This function provides a best-effort approach using gopsutil and common OS tools.
func ShowSystemInfo() {
	fmt.Println("--- System Hardware Information ---")

	// RAM (Total and Available)
	vmStat, err := mem.VirtualMemory()
	if err == nil {
		fmt.Printf("RAM: Available %.2f GB / Total %.2f GB (Used: %.2f%%)\n",
			float64(vmStat.Available)/1024/1024/1024,
			float64(vmStat.Total)/1024/1024/1024,
			vmStat.UsedPercent)
	} else {
		fmt.Printf("RAM: Error fetching - %v\n", err)
		appLogger.Printf("[SysInfo] Error fetching RAM info: %v", err)
	}

	// RAM Speed
	fmt.Println("RAM Speed:")
	printRAMSpeed()

	// CPU Model
	cpuStats, err := cpu.Info()
	if err == nil && len(cpuStats) > 0 {
		// cpuStats can have multiple entries for multi-socket systems.
		// We'll display info for the first CPU, and total logical core count.
		// Mhz is often base speed. Actual speed varies with turbo boost etc.
		fmt.Printf("CPU Model: %s (Physical Cores on first CPU: %d, Total Logical Processors: %d, Speed: %.2f GHz)\n",
			cpuStats[0].ModelName, cpuStats[0].Cores, runtime.NumCPU(), cpuStats[0].Mhz/1000.0)
	} else {
		fmt.Printf("CPU Model: Error fetching - %v\n", err)
		appLogger.Printf("[SysInfo] Error fetching CPU info: %v", err)
	}

	// GPU Model & VRAM (Total)
	fmt.Println("GPU Information:")
	printGPUInfo() // This will print model and VRAM

	// VRAM "DBus" Speed (Interpreted as VRAM Memory Clock / Bus Width / Effective Bandwidth)
	fmt.Println("VRAM Bus Info (e.g., Memory Clock, Bus Width):")
	fmt.Println("  This information is highly platform-specific and typically available via vendor-specific tools")
	fmt.Println("  (like NVIDIA Control Panel, AMD Software) or advanced system utilities with specific queries.")
	fmt.Println("  Examples for advanced users:")
	fmt.Println("  - NVIDIA (Linux/Windows): 'nvidia-smi --query-gpu=clocks.mem,memory.bus_width --format=csv'")
	fmt.Println("  - AMD (Linux): 'rocm-smi --showmeminfo vram' (may include bandwidth details), or check sysfs.")
	fmt.Println("  - AMD (Windows): AMD Radeon Software or GPU-Z.")

	fmt.Println("-----------------------------------")
	fmt.Printf("Note: System information details depend on OS (%s), drivers, and permissions.\n", runtime.GOOS)
	fmt.Println("For some details (e.g., RAM speed via dmidecode on Linux), sudo/admin rights might be needed for the commands used.")
}

func printRAMSpeed() {
	found := false
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		// dmidecode usually requires root.
		// Command: sudo dmidecode -t memory 2>/dev/null | grep -E 'Speed:.*MHz|Configured Clock Speed:.*MHz' | sed 's/^.*Speed: //;s/ MHz//;s/Configured Clock //;s/\s*$//'
		// This is complex and fragile. We'll just inform the user.
		// A simpler, non-sudo approach is usually not available for precise speed.
		fmt.Println("  Linux: Check BIOS/UEFI. For command line, try 'sudo dmidecode -t memory' and look for 'Speed' or 'Configured Clock Speed'.")
		// Example of trying to run it, but will likely fail without sudo or return nothing.
		cmd = exec.Command("sh", "-c", "dmidecode -t memory 2>/dev/null | grep -E 'Speed:.*MHz|Configured Clock Speed:.*MHz'")
		// Fallback: No standard non-root way to get this easily.
	case "windows":
		cmd = exec.Command("wmic", "memorychip", "get", "speed")
	case "darwin":
		cmd = exec.Command("system_profiler", "SPMemoryDataType")
	default:
		fmt.Printf("  %s: RAM speed detection not implemented for this OS.\n", runtime.GOOS)
		return
	}

	if cmd != nil { // If a command was set up
		output, err := cmd.Output()
		if err == nil && len(output) > 0 {
			var speeds []string
			rawOutput := string(output)

			if runtime.GOOS == "windows" {
				lines := strings.Split(strings.TrimSpace(rawOutput), "\r\n")
				if len(lines) > 1 {
					for _, line := range lines[1:] { // Skip header "Speed"
						speed := strings.TrimSpace(line)
						if speed != "" {
							speeds = append(speeds, speed+" MHz")
						}
					}
				}
			} else if runtime.GOOS == "darwin" {
				speedRegex := regexp.MustCompile(`Speed:\s*(.*)`)
				matches := speedRegex.FindAllStringSubmatch(rawOutput, -1)
				for _, match := range matches {
					if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
						speeds = append(speeds, strings.TrimSpace(match[1]))
					}
				}
			} else if runtime.GOOS == "linux" { // Basic parsing for the non-sudo dmidecode attempt
				speedRegex := regexp.MustCompile(`(?:Speed|Configured Clock Speed):\s*(\d+\s*MHz)`)
				matches := speedRegex.FindAllStringSubmatch(rawOutput, -1)
				for _, match := range matches {
					if len(match) > 1 {
						speeds = append(speeds, strings.TrimSpace(match[1]))
					}
				}
			}

			if len(speeds) > 0 {
				uniqueSpeeds := make(map[string]bool)
				var resultSpeeds []string
				for _, s := range speeds {
					if !uniqueSpeeds[s] {
						uniqueSpeeds[s] = true
						resultSpeeds = append(resultSpeeds, s)
					}
				}
				fmt.Printf("  %s (via %s): %s\n", runtime.GOOS, cmd.Args[0], strings.Join(resultSpeeds, ", "))
				found = true
			}
		} else if err != nil {
			appLogger.Printf("[SysInfo] Error running command for RAM speed '%s': %v", strings.Join(cmd.Args, " "), err)
		}
	}

	if !found {
		if runtime.GOOS == "linux" {
			// Already gave specific Linux advice.
		} else if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			fmt.Printf("  %s: Could not reliably fetch RAM speed using '%s'.\n", runtime.GOOS, cmd.Args[0])
		}
	}
	if !found && (runtime.GOOS == "linux" || runtime.GOOS == "windows" || runtime.GOOS == "darwin") {
		fmt.Println("    RAM speed may also be found in system BIOS/UEFI settings or Task Manager (Performance -> Memory on Windows).")
	}
}

func printGPUInfo() { // Prints GPU Model and VRAM (Total)
	found := false
	var cmd *exec.Cmd
	var gpuInfos []string

	switch runtime.GOOS {
	case "linux":
		// Attempt 1: nvidia-smi (for NVIDIA GPUs)
		cmd = exec.Command("nvidia-smi", "--query-gpu=gpu_name,memory.total", "--format=csv,noheader,nounits")
		output, err := cmd.Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(output)), "\n")
			for _, line := range lines {
				if line == "" {
					continue
				}
				parts := strings.Split(line, ",")
				if len(parts) == 2 {
					name := strings.TrimSpace(parts[0])
					vramMB, _ := strconv.Atoi(strings.TrimSpace(parts[1])) // Already in MiB
					gpuInfos = append(gpuInfos, fmt.Sprintf("%s (VRAM: %d MiB) [via nvidia-smi]", name, vramMB))
					found = true
				}
			}
		} else {
			appLogger.Printf("[SysInfo] nvidia-smi not found or failed: %v. Trying lspci.", err)
		}

		// Attempt 2: lspci (generic, less VRAM detail)
		if !found || len(gpuInfos) == 0 { // Try lspci if nvidia-smi failed or found nothing
			cmd = exec.Command("lspci", "-vmm")
			output, err = cmd.Output()
			if err == nil {
				currentDevice := make(map[string]string)
				var lspci_gpus []string
				for _, line := range strings.Split(string(output), "\n") {
					if line == "" && len(currentDevice) > 0 {
						if class, ok := currentDevice["Class"]; ok && (strings.Contains(class, "VGA compatible controller") || strings.Contains(class, "3D controller") || strings.Contains(class, "Display controller")) {
							name := currentDevice["Device"]
							if vendor, vOk := currentDevice["Vendor"]; vOk {
								name = vendor + " " + name
							}
							lspci_gpus = append(lspci_gpus, name+" [via lspci]")
						}
						currentDevice = make(map[string]string)
						continue
					}
					parts := strings.SplitN(line, ":\t", 2)
					if len(parts) == 2 {
						currentDevice[parts[0]] = strings.TrimSpace(parts[1])
					}
				}
				if len(lspci_gpus) > 0 {
					gpuInfos = append(gpuInfos, lspci_gpus...)
					found = true
				}
			} else {
				appLogger.Printf("[SysInfo] lspci failed: %v", err)
			}
		}
		if !found {
			fmt.Println("  Linux: GPU detection failed. Try 'nvidia-smi', 'lspci | grep -E \"VGA|3D\"', or 'rocm-smi'.")
		}

	case "windows":
		cmd = exec.Command("wmic", "path", "Win32_VideoController", "get", "Name,AdapterRAM,DriverVersion", "/FORMAT:CSV")
		output, err := cmd.Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(output)), "\r\n")
			if len(lines) > 1 {
				for i, line := range lines {
					if i == 0 || line == "" {
						continue
					}
					parts := strings.Split(line, ",")
					// CSV format from this WMIC query: Node,AdapterRAM,DriverVersion,Name
					if len(parts) >= 4 {
						name := strings.TrimSpace(parts[3])
						adapterRAMStr := strings.TrimSpace(parts[1])
						vramBytes, convErr := strconv.ParseInt(adapterRAMStr, 10, 64)
						if convErr == nil {
							gpuInfos = append(gpuInfos, fmt.Sprintf("%s (VRAM: %.2f GB) [via WMIC]", name, float64(vramBytes)/1024/1024/1024))
						} else {
							gpuInfos = append(gpuInfos, fmt.Sprintf("%s (VRAM: %s [raw]) [via WMIC]", name, adapterRAMStr))
						}
						found = true
					}
				}
			}
		} else {
			appLogger.Printf("[SysInfo] WMIC for GPU failed: %v", err)
		}
		if !found {
			fmt.Println("  Windows: GPU detection via WMIC failed. Check Task Manager (Performance tab) or 'dxdiag'.")
		}

	case "darwin":
		cmd = exec.Command("system_profiler", "SPDisplaysDataType")
		output, err := cmd.Output()
		if err == nil {
			content := string(output)
			// Simplified parsing for macOS. `system_profiler` output is complex.
			chipsetModelRegex := regexp.MustCompile(`Chipset Model:\s*(.*)`)
			vramRegex := regexp.MustCompile(`VRAM (?:\(Total\)|(?:Dynamic, Max)):\s*(.*)`)

			// Look for blocks of GPU info. A common way is to split by "Graphics/Displays:",
			// then look for "Chipset Model:" sections.
			// This is a simplified sequential scan which might miss complex multi-GPU setups if not distinctly separated.
			modelMatches := chipsetModelRegex.FindAllStringSubmatch(content, -1)
			vramMatches := vramRegex.FindAllStringSubmatch(content, -1)

			numGpus := len(modelMatches)
			if numGpus > 0 {
				for i := 0; i < numGpus; i++ {
					model := "N/A"
					vram := "N/A"
					if len(modelMatches[i]) > 1 {
						model = strings.TrimSpace(modelMatches[i][1])
					}
					// Try to associate VRAM if available; this matching is loose.
					if i < len(vramMatches) && len(vramMatches[i]) > 1 {
						vram = strings.TrimSpace(vramMatches[i][1])
					}
					gpuInfos = append(gpuInfos, fmt.Sprintf("%s (VRAM: %s) [via system_profiler]", model, vram))
					found = true
				}
			}
		} else {
			appLogger.Printf("[SysInfo] system_profiler for GPU failed: %v", err)
		}
		if !found {
			fmt.Println("  macOS: GPU detection via system_profiler failed. Check 'About This Mac -> System Report -> Graphics/Displays'.")
		}

	default:
		fmt.Printf("  %s: GPU/VRAM info detection not implemented for this OS.\n", runtime.GOOS)
	}

	if len(gpuInfos) > 0 {
		for _, info := range gpuInfos {
			fmt.Printf("  - %s\n", info)
		}
	} else if found { // Found flag set but no info string populated, indicates parsing issue
		fmt.Println("  Could parse some GPU information, but no concrete details extracted.")
	}
	// If !found and specific OS message wasn't printed, this will be the fallback:
	if !found && (runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "darwin") {
		fmt.Println("  GPU information requires OS-specific tools not attempted for this platform.")
	}
}
