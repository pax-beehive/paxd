package machineinfo

import (
	"errors"
	"os"
	"testing"
)

func TestDetectDarwinUsesMachineName(t *testing.T) {
	name := detectDarwin(func(command string, args ...string) ([]byte, error) {
		if command != "system_profiler" {
			return nil, errors.New("unexpected command")
		}
		return []byte(`{"SPHardwareDataType":[{"machine_name":"MacBook Pro","machine_model":"MacBookPro16,1"}]}`), nil
	})

	if name != "MacBook Pro" {
		t.Fatalf("name = %q, want MacBook Pro", name)
	}
}

func TestDetectLinuxDoesNotGuessFromUnreliableChassisType(t *testing.T) {
	files := map[string]string{
		"/sys/class/dmi/id/sys_vendor":   "$(DEFAULT_STRING)\n",
		"/sys/class/dmi/id/product_name": "$(DEFAULT_STRING)\n",
		"/sys/class/dmi/id/board_vendor": "$(DEFAULT_STRING)\n",
		"/sys/class/dmi/id/board_name":   "$(DEFAULT_STRING)\n",
		"/sys/class/dmi/id/chassis_type": "3\n",
		"/proc/device-tree/model":        "",
	}

	name := detectLinux(func(path string) ([]byte, error) {
		value, ok := files[path]
		if !ok || value == "" {
			return nil, os.ErrNotExist
		}
		return []byte(value), nil
	})

	if name != "Computer" {
		t.Fatalf("name = %q, want Computer", name)
	}
}

func TestDetectLinuxUsesProductThenCustomBoard(t *testing.T) {
	t.Run("product", func(t *testing.T) {
		files := map[string]string{
			"/sys/class/dmi/id/sys_vendor":   "Dell Inc.",
			"/sys/class/dmi/id/product_name": "Precision 3660 Tower",
		}
		name := detectLinux(mapReader(files))
		if name != "Dell Inc. Precision 3660 Tower" {
			t.Fatalf("name = %q", name)
		}
	})

	t.Run("custom board", func(t *testing.T) {
		files := map[string]string{
			"/sys/class/dmi/id/board_vendor": "ASUSTeK",
			"/sys/class/dmi/id/board_name":   "ROG STRIX B650E-E",
			"/sys/class/dmi/id/chassis_type": "3",
		}
		name := detectLinux(mapReader(files))
		if name != "ASUSTeK ROG STRIX B650E-E" {
			t.Fatalf("name = %q", name)
		}
	})
}

func TestDetectWindowsUsesCIMModel(t *testing.T) {
	name := detectWindows(func(command string, args ...string) ([]byte, error) {
		return []byte(`{"Manufacturer":"LENOVO","Model":"ThinkPad P1 Gen 7"}`), nil
	})

	if name != "LENOVO ThinkPad P1 Gen 7" {
		t.Fatalf("name = %q", name)
	}
}

func mapReader(files map[string]string) readFileFunc {
	return func(path string) ([]byte, error) {
		value, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(value), nil
	}
}
