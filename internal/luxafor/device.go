// Package luxafor drives a Luxafor Flag (USB VID 04D8, PID F372) via
// raw hidraw writes on Linux. No cgo, no external deps.
package luxafor

import (
	"fmt"
	"os"
	"sync"
)

// USB identifiers for the Luxafor Flag.
const (
	VendorID  = 0x04D8
	ProductID = 0xF372
)

// HID report layout (9 bytes on the wire: 1-byte report ID + 8-byte payload).
//
//	[0] = 0x00       report ID — no numbered reports, so always zero.
//	[1] = command    0x01 = set color immediately.
//	[2] = target     0xFF = all LEDs.
//	[3..5] = R, G, B
//	[6..8] = 0       reserved / unused for this command.
const (
	cmdSetColor byte = 0x01
	targetAll   byte = 0xFF
)

// Device is an opened /dev/hidrawN handle for the Flag.
// Method calls are serialized so concurrent SetColor is safe.
type Device struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// Open opens the hidraw node at path for writing.
func Open(path string) (*Device, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Device{path: path, f: f}, nil
}

// Path returns the hidraw node path this device was opened from.
func (d *Device) Path() string { return d.path }

// SetColor sets every LED to the given RGB value.
func (d *Device) SetColor(r, g, b byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return fmt.Errorf("device closed")
	}
	buf := [9]byte{0x00, cmdSetColor, targetAll, r, g, b, 0, 0, 0}
	_, err := d.f.Write(buf[:])
	return err
}

// Close releases the hidraw handle.
func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}
