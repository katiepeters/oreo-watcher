// Package bthome decodes BTHome v2 BLE advertisement service data
// (https://bthome.io/format), the format Shelly BLU buttons broadcast.
package bthome

import (
	"errors"
	"fmt"
)

// ServiceUUID is the 128-bit form of BTHome's 16-bit service UUID 0xFCD2, as
// BlueZ reports it in a device's ServiceData.
const ServiceUUID = "0000fcd2-0000-1000-8000-00805f9b34fb"

// ButtonEvent is the value of a BTHome button object (0x3A).
type ButtonEvent uint8

const (
	ButtonNone        ButtonEvent = 0x00
	ButtonPress       ButtonEvent = 0x01
	ButtonDoublePress ButtonEvent = 0x02
	ButtonTriplePress ButtonEvent = 0x03
	ButtonLongPress   ButtonEvent = 0x04
)

// ErrEncrypted is returned for adverts whose payload is AES-CCM encrypted.
var ErrEncrypted = errors.New("bthome: advert is encrypted; encryption is not supported")

const (
	deviceInfoEncrypted   = 0x01
	deviceInfoMACIncluded = 0x02
	deviceInfoVersionMask = 0xE0
	deviceInfoVersion2    = 0x40

	objectPacketID = 0x00
	objectBattery  = 0x01
	objectButton   = 0x3A
)

// objectLengths is the value size in bytes of each BTHome object this decoder
// can step over. An ID missing from here ends parsing, since without its
// length the objects after it can't be located.
var objectLengths = map[byte]int{
	objectPacketID: 1,
	objectBattery:  1,
	0x02:           2, // temperature
	0x03:           2, // humidity
	0x04:           3, // pressure
	0x05:           3, // illuminance
	0x0C:           2, // voltage
	0x21:           1, // motion
	0x2D:           1, // window
	0x2E:           1, // humidity
	objectButton:   1,
	0x3C:           2, // dimmer
	0x3F:           2, // rotation
	0x45:           2, // temperature
	0xF0:           2, // device type id
	0xF1:           4, // firmware version
	0xF2:           3, // firmware version
}

// Packet is the decoded content of one advert.
type Packet struct {
	// PacketID is the sender's revolving counter; repeats of one advert share
	// it. Only meaningful when HasPacketID is true.
	PacketID    uint8
	HasPacketID bool
	// BatteryPercent is 0-100. Only meaningful when HasBattery is true.
	BatteryPercent uint8
	HasBattery     bool
	// Buttons holds one event per button object in advert order, so a
	// four-button remote always yields four entries (index 0 = button 1).
	Buttons []ButtonEvent
}

// Parse decodes the BTHome service data of one advert (the bytes following the
// 0xFCD2 UUID). Objects before an unrecognized object ID are still returned
// alongside the error.
func Parse(data []byte) (Packet, error) {
	var p Packet
	if len(data) < 1 {
		return p, errors.New("bthome: empty service data")
	}
	info := data[0]
	if info&deviceInfoVersionMask != deviceInfoVersion2 {
		return p, fmt.Errorf("bthome: unsupported version in device info byte 0x%02X", info)
	}
	if info&deviceInfoEncrypted != 0 {
		return p, ErrEncrypted
	}
	rest := data[1:]
	if info&deviceInfoMACIncluded != 0 {
		if len(rest) < 6 {
			return p, errors.New("bthome: truncated MAC address")
		}
		rest = rest[6:]
	}
	for len(rest) > 0 {
		id := rest[0]
		size, ok := objectLengths[id]
		if !ok {
			return p, fmt.Errorf("bthome: unknown object ID 0x%02X", id)
		}
		if len(rest) < 1+size {
			return p, fmt.Errorf("bthome: truncated object 0x%02X", id)
		}
		value := rest[1 : 1+size]
		switch id {
		case objectPacketID:
			p.PacketID, p.HasPacketID = value[0], true
		case objectBattery:
			p.BatteryPercent, p.HasBattery = value[0], true
		case objectButton:
			p.Buttons = append(p.Buttons, ButtonEvent(value[0]))
		}
		rest = rest[1+size:]
	}
	return p, nil
}
