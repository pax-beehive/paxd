package machineinfo

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

type commandOutputFunc func(name string, args ...string) ([]byte, error)
type readFileFunc func(name string) ([]byte, error)

var cachedName = sync.OnceValue(func() string {
	return detect(runtime.GOOS, os.ReadFile, exec.Command)
})

// Name returns a best-effort, human-readable hardware name without collecting
// serial numbers or other stable device identifiers.
func Name() string {
	return cachedName()
}

// Resolve preserves an explicit useful name and replaces empty or placeholder
// configuration with the detected hardware name.
func Resolve(configured string) string {
	if value := usefulValue(configured); value != "" {
		return value
	}
	return Name()
}

func detect(goos string, readFile readFileFunc, command func(name string, args ...string) *exec.Cmd) string {
	output := func(name string, args ...string) ([]byte, error) {
		return command(name, args...).Output()
	}
	switch goos {
	case "darwin":
		return detectDarwin(output)
	case "linux":
		return detectLinux(readFile)
	case "windows":
		return detectWindows(output)
	default:
		return "Computer"
	}
}

func detectDarwin(output commandOutputFunc) string {
	data, err := output("system_profiler", "SPHardwareDataType", "-json")
	if err == nil {
		var report struct {
			Hardware []struct {
				MachineName  string `json:"machine_name"`
				MachineModel string `json:"machine_model"`
			} `json:"SPHardwareDataType"`
		}
		if json.Unmarshal(data, &report) == nil && len(report.Hardware) > 0 {
			if name := usefulValue(report.Hardware[0].MachineName); name != "" {
				return name
			}
			if model := usefulValue(report.Hardware[0].MachineModel); model != "" {
				return model
			}
		}
	}
	if data, err := output("sysctl", "-n", "hw.model"); err == nil {
		if model := usefulValue(string(data)); model != "" {
			return model
		}
	}
	return "Mac"
}

func detectLinux(readFile readFileFunc) string {
	read := func(path string) string {
		data, err := readFile(path)
		if err != nil {
			return ""
		}
		return usefulValue(string(data))
	}

	vendor := read("/sys/class/dmi/id/sys_vendor")
	product := read("/sys/class/dmi/id/product_name")
	if product != "" {
		return joinVendorModel(vendor, product)
	}
	if model := read("/proc/device-tree/model"); model != "" {
		return model
	}

	board := joinVendorModel(
		read("/sys/class/dmi/id/board_vendor"),
		read("/sys/class/dmi/id/board_name"),
	)
	if board != "" {
		return board
	}
	return "Computer"
}

func detectWindows(output commandOutputFunc) string {
	const script = `$computer = Get-CimInstance Win32_ComputerSystem; ` +
		`[PSCustomObject]@{Manufacturer=$computer.Manufacturer;Model=$computer.Model} | ` +
		`ConvertTo-Json -Compress`
	for _, executable := range []string{"powershell.exe", "pwsh.exe"} {
		data, err := output(executable, "-NoProfile", "-NonInteractive", "-Command", script)
		if err != nil {
			continue
		}
		var report struct {
			Manufacturer string `json:"Manufacturer"`
			Model        string `json:"Model"`
		}
		if json.Unmarshal(data, &report) != nil {
			continue
		}
		if model := usefulValue(report.Model); model != "" {
			return joinVendorModel(usefulValue(report.Manufacturer), model)
		}
	}
	return "Computer"
}

func joinVendorModel(vendor, model string) string {
	vendor = usefulValue(vendor)
	model = usefulValue(model)
	if model == "" {
		return ""
	}
	if vendor == "" || strings.Contains(strings.ToLower(model), strings.ToLower(vendor)) {
		return model
	}
	return vendor + " " + model
}

func usefulValue(value string) string {
	value = strings.Join(strings.Fields(strings.Trim(value, "\x00 \t\r\n")), " ")
	canonical := strings.ToLower(value)
	canonical = strings.NewReplacer(
		"$", "", "(", "", ")", "", "_", "", "-", "", ".", "", " ", "",
	).Replace(canonical)
	switch canonical {
	case "", "unknown", "none", "na", "notavailable", "notapplicable",
		"defaultstring", "systemproductname", "tobefilledbyoem", "oem":
		return ""
	default:
		return value
	}
}
