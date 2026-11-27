package bthome

import (
	"errors"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		want    Packet
		wantErr bool
	}{
		{
			name: "four-button remote, button 3 pressed",
			data: []byte{0x44, 0x00, 0x2A, 0x01, 0x64, 0x3A, 0x00, 0x3A, 0x00, 0x3A, 0x01, 0x3A, 0x00},
			want: Packet{
				PacketID: 0x2A, HasPacketID: true,
				BatteryPercent: 100, HasBattery: true,
				Buttons: []ButtonEvent{ButtonNone, ButtonNone, ButtonPress, ButtonNone},
			},
		},
		{
			name: "periodic beacon with firmware objects and no buttons",
			data: []byte{0x40, 0x00, 0x07, 0x01, 0x5A, 0xF0, 0x16, 0x00, 0xF1, 0x01, 0x02, 0x03, 0x04},
			want: Packet{PacketID: 7, HasPacketID: true, BatteryPercent: 90, HasBattery: true},
		},
		{
			name: "MAC included in payload is skipped",
			data: []byte{0x46, 1, 2, 3, 4, 5, 6, 0x00, 0x01, 0x3A, 0x02},
			want: Packet{PacketID: 1, HasPacketID: true, Buttons: []ButtonEvent{ButtonDoublePress}},
		},
		{
			name:    "unknown object keeps what was decoded before it",
			data:    []byte{0x44, 0x00, 0x09, 0x99, 0x01},
			want:    Packet{PacketID: 9, HasPacketID: true},
			wantErr: true,
		},
		{name: "truncated object", data: []byte{0x44, 0x00}, wantErr: true},
		{name: "empty", data: nil, wantErr: true},
		{name: "BTHome v1", data: []byte{0x02, 0x00, 0x01}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.data)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseEncrypted(t *testing.T) {
	if _, err := Parse([]byte{0x45, 0xDE, 0xAD, 0xBE, 0xEF}); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("err = %v, want ErrEncrypted", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte{0x44, 0x00, 0x2A, 0x01, 0x64, 0x3A, 0x00, 0x3A, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		p, _ := Parse(data)
		if len(p.Buttons) > len(data)/2 {
			t.Errorf("decoded %d buttons from %d bytes", len(p.Buttons), len(data))
		}
	})
}
