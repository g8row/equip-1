package ble

import (
	"context"
	"fmt"

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

type characteristic struct {
	path        dbus.ObjectPath
	uuid        string
	servicePath dbus.ObjectPath
	flags       []string
	name        string
	value       []byte
	notifying   bool

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
		props["Notifying"] = dbus.MakeVariant(c.notifying)
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

func (c *characteristic) ReadValue(options map[string]dbus.Variant) ([]byte, *dbus.Error) {
	if c.read == nil {
		return c.value, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2_000_000_000)
	defer cancel()
	return c.read(ctx)
}

func (c *characteristic) WriteValue(value []byte, options map[string]dbus.Variant) *dbus.Error {
	if c.write == nil {
		c.value = append(c.value[:0], value...)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5_000_000_000)
	defer cancel()
	return c.write(ctx, value)
}

func (c *characteristic) StartNotify() *dbus.Error {
	if !hasFlag(c.flags, "notify") {
		return dbus.MakeFailedError(fmt.Errorf("characteristic is not notifiable"))
	}
	c.notifying = true
	return nil
}

func (c *characteristic) StopNotify() *dbus.Error {
	c.notifying = false
	return nil
}

func (c *characteristic) emitValue(conn *dbus.Conn, value []byte) {
	c.value = append(c.value[:0], value...)
	changed := map[string]dbus.Variant{"Value": dbus.MakeVariant(c.value)}
	invalidated := []string{}
	_ = conn.Emit(c.path, ifaceProperties+".PropertiesChanged", ifaceGattCharacteristic, changed, invalidated)
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
