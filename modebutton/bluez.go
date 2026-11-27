package modebutton

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/katie-viam/oreo-watcher/bthome"
)

// advert is one BTHome broadcast heard from a device.
type advert struct {
	address string // upper-case, colon-separated MAC
	data    []byte // BTHome service data
	// cached marks a value BlueZ remembered from before listening began. It
	// may be minutes old, so it must never be treated as a fresh button press.
	cached bool
}

// advertSource delivers BTHome adverts to onAdvert until ctx is cancelled
// (returning nil) or listening fails.
type advertSource func(ctx context.Context, onAdvert func(advert)) error

const (
	bluezService    = "org.bluez"
	bluezAdapter    = "org.bluez.Adapter1"
	bluezDevice     = "org.bluez.Device1"
	dbusProperties  = "org.freedesktop.DBus.Properties"
	dbusObjectMgr   = "org.freedesktop.DBus.ObjectManager"
	sigPropsChanged = dbusProperties + ".PropertiesChanged"
	sigIfacesAdded  = dbusObjectMgr + ".InterfacesAdded"
)

// listenBlueZ returns an advertSource backed by BlueZ's LE discovery on the
// named adapter (e.g. "hci0"). BlueZ doesn't expose raw advertising packets;
// it surfaces them as changes to each device's ServiceData property.
func listenBlueZ(adapter string) advertSource {
	return func(ctx context.Context, onAdvert func(advert)) error {
		conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("connecting to the system D-Bus (is BlueZ installed and running?): %w", err)
		}
		defer conn.Close()

		adapterPath := dbus.ObjectPath("/org/bluez/" + adapter)
		adapterObj := conn.Object(bluezService, adapterPath)

		signals := make(chan *dbus.Signal, 64)
		conn.Signal(signals)
		for _, match := range [][]dbus.MatchOption{
			{dbus.WithMatchSender(bluezService), dbus.WithMatchInterface(dbusProperties), dbus.WithMatchMember("PropertiesChanged")},
			{dbus.WithMatchSender(bluezService), dbus.WithMatchInterface(dbusObjectMgr), dbus.WithMatchMember("InterfacesAdded")},
		} {
			if err := conn.AddMatchSignal(match...); err != nil {
				return fmt.Errorf("subscribing to BlueZ signals: %w", err)
			}
		}

		var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
		if err := conn.Object(bluezService, "/").
			CallWithContext(ctx, dbusObjectMgr+".GetManagedObjects", 0).Store(&objects); err != nil {
			return fmt.Errorf("listing BlueZ objects: %w", err)
		}
		if _, ok := objects[adapterPath]; !ok {
			return fmt.Errorf("bluetooth adapter %q not found", adapter)
		}
		for path, ifaces := range objects {
			if a, ok := advertFrom(adapterPath, path, ifaces[bluezDevice]); ok {
				a.cached = true
				onAdvert(a)
			}
		}

		// DuplicateData makes BlueZ signal ServiceData every time it is heard
		// rather than only when it differs from the previous value.
		filter := map[string]interface{}{"Transport": "le", "DuplicateData": true}
		if err := adapterObj.CallWithContext(ctx, bluezAdapter+".SetDiscoveryFilter", 0, filter).Err; err != nil {
			return fmt.Errorf("setting BlueZ discovery filter: %w", err)
		}
		if err := adapterObj.CallWithContext(ctx, bluezAdapter+".StartDiscovery", 0).Err; err != nil {
			return fmt.Errorf("starting BlueZ discovery (is the adapter powered on?): %w", err)
		}
		defer adapterObj.Call(bluezAdapter+".StopDiscovery", 0)

		for {
			select {
			case <-ctx.Done():
				return nil
			case sig, ok := <-signals:
				if !ok {
					return errors.New("D-Bus connection closed")
				}
				switch sig.Name {
				case sigIfacesAdded:
					if len(sig.Body) < 2 {
						continue
					}
					path, _ := sig.Body[0].(dbus.ObjectPath)
					ifaces, _ := sig.Body[1].(map[string]map[string]dbus.Variant)
					if a, ok := advertFrom(adapterPath, path, ifaces[bluezDevice]); ok {
						onAdvert(a)
					}
				case sigPropsChanged:
					if len(sig.Body) < 2 {
						continue
					}
					iface, _ := sig.Body[0].(string)
					changes, _ := sig.Body[1].(map[string]dbus.Variant)
					switch iface {
					case bluezAdapter:
						if sig.Path != adapterPath {
							continue
						}
						if discovering, ok := changes["Discovering"].Value().(bool); ok && !discovering {
							return errors.New("BlueZ stopped discovery")
						}
					case bluezDevice:
						if a, ok := advertFrom(adapterPath, sig.Path, changes); ok {
							onAdvert(a)
						}
					}
				}
			}
		}
	}
}

// advertFrom extracts a BTHome advert from a BlueZ device's properties (either
// its full property set or the changed subset in a PropertiesChanged signal).
func advertFrom(adapterPath, devicePath dbus.ObjectPath, props map[string]dbus.Variant) (advert, bool) {
	address, ok := addressFromPath(adapterPath, devicePath)
	if !ok {
		return advert{}, false
	}
	serviceData, ok := props["ServiceData"].Value().(map[string]dbus.Variant)
	if !ok {
		return advert{}, false
	}
	data, ok := serviceData[bthome.ServiceUUID].Value().([]byte)
	if !ok {
		return advert{}, false
	}
	return advert{address: address, data: data}, true
}

// addressFromPath recovers a device's MAC from its BlueZ object path, e.g.
// /org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF -> AA:BB:CC:DD:EE:FF.
func addressFromPath(adapterPath, devicePath dbus.ObjectPath) (string, bool) {
	mac, ok := strings.CutPrefix(string(devicePath), string(adapterPath)+"/dev_")
	if !ok || len(mac) != 17 || strings.Contains(mac, "/") {
		return "", false
	}
	return strings.ToUpper(strings.ReplaceAll(mac, "_", ":")), true
}
