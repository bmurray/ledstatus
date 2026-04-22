package luxafor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Discover walks /sys/class/hidraw and returns the /dev/hidrawN path whose
// HID_ID matches Luxafor's VID/PID. Empty path + nil error means no device.
func Discover() (string, error) {
	const sysDir = "/sys/class/hidraw"
	entries, err := os.ReadDir(sysDir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", sysDir, err)
	}
	for _, e := range entries {
		ueventPath := filepath.Join(sysDir, e.Name(), "device", "uevent")
		data, err := os.ReadFile(ueventPath)
		if err != nil {
			continue
		}
		vid, pid, ok := parseHIDID(string(data))
		if !ok {
			continue
		}
		if vid == VendorID && pid == ProductID {
			return "/dev/" + e.Name(), nil
		}
	}
	return "", nil
}

// parseHIDID reads the HID_ID=bus:vid:pid line from a uevent file.
// Values are hex, zero-padded to 8 chars in the kernel's format.
func parseHIDID(uevent string) (vid, pid uint16, ok bool) {
	for _, line := range strings.Split(uevent, "\n") {
		rest, found := strings.CutPrefix(line, "HID_ID=")
		if !found {
			continue
		}
		parts := strings.Split(rest, ":")
		if len(parts) != 3 {
			return 0, 0, false
		}
		v, err1 := strconv.ParseUint(parts[1], 16, 32)
		p, err2 := strconv.ParseUint(parts[2], 16, 32)
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return uint16(v), uint16(p), true
	}
	return 0, 0, false
}
