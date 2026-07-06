package ble

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

type managedObject interface {
	Path() dbus.ObjectPath
	Interfaces() map[string]map[string]dbus.Variant
	GetAll(iface string) (map[string]dbus.Variant, *dbus.Error)
}

type application struct {
	objects []managedObject
}

func (a *application) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	objects := make(map[dbus.ObjectPath]map[string]map[string]dbus.Variant, len(a.objects))
	for _, obj := range a.objects {
		objects[obj.Path()] = obj.Interfaces()
	}
	return objects, nil
}

type gattService struct {
	path dbus.ObjectPath
}

func (s *gattService) Path() dbus.ObjectPath { return s.path }

func (s *gattService) Interfaces() map[string]map[string]dbus.Variant {
	return map[string]map[string]dbus.Variant{
		ifaceGattService: {
			"UUID":    dbus.MakeVariant(serviceUUID),
			"Primary": dbus.MakeVariant(true),
		},
	}
}

func (s *gattService) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	props, ok := s.Interfaces()[iface]
	if !ok {
		return nil, unknownInterface(iface)
	}
	return props, nil
}

// notifyMaxBytes caps PropertiesChanged notification payloads. BlueZ will
// fragment a notify larger than MTU-3, but many phones (iOS in particular)
// never negotiate above the default ATT MTU of 23, leaving only 20 usable
// bytes; anything larger than that risks silent truncation instead of a
// clean fragmentation. Full-size JSON bodies (e.g. multi-KB wifi_scan or
// status) are therefore never sent over notify — a notification only signals
// "something changed"; the client must re-read the characteristic (which
// supports the offset-based multi-part read in ReadValue below) to fetch the
// full value.
const notifyMaxBytes = 20

type characteristic struct {
	path        dbus.ObjectPath
	uuid        string
	servicePath dbus.ObjectPath
	flags       []string
	name        string
	notifying   bool

	// mu guards value and readBuf: godbus dispatches D-Bus method calls
	// (ReadValue/WriteValue) concurrently, and emitValue runs from the
	// server's notify-loop goroutine, so without a lock emitValue's update of
	// value could race a concurrent ReadValue's snapshot/marshal.
	mu      sync.Mutex
	value   []byte
	readBuf []byte // snapshot served across a multi-part (offset>0) read

	read  func(context.Context) ([]byte, *dbus.Error)
	write func(context.Context, []byte) *dbus.Error
}

func newCharacteristic(uuid string, servicePath dbus.ObjectPath, name string, flags []string) *characteristic {
	return &characteristic{
		path:        dbus.ObjectPath(fmt.Sprintf("%s/%s", servicePath, name)),
		uuid:        uuid,
		servicePath: servicePath,
		flags:       flags,
		name:        name,
		value:       []byte("{}"),
	}
}

func (c *characteristic) Path() dbus.ObjectPath { return c.path }

func (c *characteristic) Interfaces() map[string]map[string]dbus.Variant {
	props := map[string]dbus.Variant{
		"UUID":    dbus.MakeVariant(c.uuid),
		"Service": dbus.MakeVariant(c.servicePath),
		"Flags":   dbus.MakeVariant(c.flags),
	}
	if hasFlag(c.flags, "notify") {
		props["Notifying"] = dbus.MakeVariant(c.IsNotifying())
	}
	return map[string]map[string]dbus.Variant{ifaceGattCharacteristic: props}
}

func (c *characteristic) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	props, ok := c.Interfaces()[iface]
	if !ok {
		return nil, unknownInterface(iface)
	}
	return props, nil
}

// Value returns a copy of the characteristic's current value. Safe for
// concurrent use.
func (c *characteristic) Value() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, len(c.value))
	copy(out, c.value)
	return out
}

// setValue replaces the characteristic's stored value with a copy of v.
func (c *characteristic) setValue(v []byte) {
	c.mu.Lock()
	c.value = append(c.value[:0:0], v...)
	c.mu.Unlock()
}

// IsNotifying reports whether a client has subscribed via StartNotify. Safe
// for concurrent use.
func (c *characteristic) IsNotifying() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notifying
}

// readOffset extracts the "offset" option BlueZ passes on ReadValue for
// multi-part reads (a client's negotiated MTU may be smaller than the full
// value, e.g. wifi_scan JSON on iOS's default MTU of 23).
func readOffset(options map[string]dbus.Variant) uint16 {
	v, ok := options["offset"]
	if !ok {
		return 0
	}
	off, ok := v.Value().(uint16)
	if !ok {
		return 0
	}
	return off
}

// ReadValue implements org.bluez.GattCharacteristic1.ReadValue, honoring the
// "offset" option BlueZ uses to fetch a value in multiple parts when it is
// larger than ATT_MTU-1. On offset 0 it takes a fresh snapshot (via the read
// callback, if any) and serves subsequent offsets from that same snapshot so
// a multi-part read sequence is internally consistent even if the underlying
// value changes mid-sequence.
func (c *characteristic) ReadValue(options map[string]dbus.Variant) ([]byte, *dbus.Error) {
	offset := readOffset(options)

	c.mu.Lock()
	defer c.mu.Unlock()

	if offset == 0 {
		if c.read != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			fresh, err := c.read(ctx)
			cancel()
			if err != nil {
				return nil, err
			}
			c.readBuf = append(c.readBuf[:0:0], fresh...)
		} else {
			c.readBuf = append(c.readBuf[:0:0], c.value...)
		}
	}

	if int(offset) > len(c.readBuf) {
		return nil, dbus.NewError("org.bluez.Error.InvalidOffset", nil)
	}

	out := make([]byte, len(c.readBuf)-int(offset))
	copy(out, c.readBuf[offset:])
	return out, nil
}

func (c *characteristic) WriteValue(value []byte, options map[string]dbus.Variant) *dbus.Error {
	if c.write == nil {
		c.setValue(value)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.write(ctx, value)
}

func (c *characteristic) StartNotify() *dbus.Error {
	if !hasFlag(c.flags, "notify") {
		return dbus.MakeFailedError(fmt.Errorf("characteristic is not notifiable"))
	}
	c.mu.Lock()
	c.notifying = true
	c.mu.Unlock()
	return nil
}

func (c *characteristic) StopNotify() *dbus.Error {
	c.mu.Lock()
	c.notifying = false
	c.mu.Unlock()
	return nil
}

// emitValue stores value as the characteristic's current value (readable in
// full, including via multi-part ReadValue) and emits a PropertiesChanged
// notification carrying a copy truncated to notifyMaxBytes — see its doc
// comment for why. Callers that need the client to see the whole value must
// have it re-read the characteristic on notify.
func (c *characteristic) emitValue(conn *dbus.Conn, value []byte) {
	c.setValue(value)
	notifyPayload := truncateForNotify(c.Value())
	changed := map[string]dbus.Variant{"Value": dbus.MakeVariant(notifyPayload)}
	invalidated := []string{}
	_ = conn.Emit(c.path, ifaceProperties+".PropertiesChanged", ifaceGattCharacteristic, changed, invalidated)
}

func truncateForNotify(value []byte) []byte {
	if len(value) <= notifyMaxBytes {
		return value
	}
	return value[:notifyMaxBytes]
}

func hasFlag(flags []string, needle string) bool {
	for _, flag := range flags {
		if flag == needle {
			return true
		}
	}
	return false
}

func unknownInterface(iface string) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.UnknownInterface", []any{iface})
}

type advertisement struct {
	path         dbus.ObjectPath
	localName    string
	serviceUUIDs []string
}

func (a *advertisement) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != ifaceAdvertisement {
		return nil, unknownInterface(iface)
	}
	return map[string]dbus.Variant{
		"Type":         dbus.MakeVariant("peripheral"),
		"ServiceUUIDs": dbus.MakeVariant(a.serviceUUIDs),
		"LocalName":    dbus.MakeVariant(a.localName),
		"Includes":     dbus.MakeVariant([]string{"tx-power"}),
	}, nil
}

func (a *advertisement) Release() {}
