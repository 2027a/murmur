package ui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"git.2027a.net/mathieu/murmur/internal/events"
)

const notificationBuffer = 16

type Notification struct {
	Summary   string
	Body      string
	Urgency   byte
	TimeoutMS int32
}

type NotificationClient interface {
	Notify(ctx context.Context, replacesID uint32, notification Notification) (uint32, error)
	Close() error
}

type Notify struct {
	client NotificationClient
	warn   WarnFunc
	queue  chan Notification
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func NewNotify(client NotificationClient, warn WarnFunc) *Notify {
	u := &Notify{
		client: client,
		warn:   warn,
		queue:  make(chan Notification, notificationBuffer),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go u.run()
	return u
}

// NewDesktopNotifications degrades to the no-op UI when the session bus is absent.
func NewDesktopNotifications(warn WarnFunc) UI {
	client, err := NewDBusNotificationClient()
	if err != nil {
		warning(warn, "notifications D-Bus indisponibles: %v", err)
		return NewNone()
	}
	return NewNotify(client, warn)
}

func (u *Notify) Show(event events.Event) {
	if u == nil || u.client == nil {
		return
	}
	notification, ok := notificationForEvent(event)
	if !ok {
		return
	}
	select {
	case <-u.stop:
		return
	default:
	}
	// Keep recent state changes when the desktop service is slow.
	select {
	case u.queue <- notification:
	default:
		select {
		case <-u.queue:
		default:
		}
		select {
		case u.queue <- notification:
		default:
		}
	}
}

func (u *Notify) Close() {
	if u == nil {
		return
	}
	u.once.Do(func() {
		close(u.stop)
		<-u.done
	})
}

func (u *Notify) run() {
	defer close(u.done)
	defer u.client.Close()
	var replacesID uint32
	for {
		select {
		case <-u.stop:
			return
		case notification := <-u.queue:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			id, err := u.client.Notify(ctx, replacesID, notification)
			cancel()
			if err != nil {
				warning(u.warn, "notification ignoree: %v", err)
				continue
			}
			replacesID = id
		}
	}
}

func notificationForEvent(event events.Event) (Notification, bool) {
	n := Notification{Summary: "Murmur", Urgency: 1, TimeoutMS: 1800}
	switch event.Type {
	case events.SessionStarted:
		n.Body = "Demarrage de la capture"
	case events.CaptureReady:
		n.Body = "Ecoute en cours"
		n.TimeoutMS = 0
	case events.StateChanged:
		switch event.State {
		case "stopping":
			n.Body = "Arret de la capture"
		case "transcribing":
			n.Body = "Transcription en cours"
		case "processing":
			n.Body = "Traitement en cours"
		case "delivering":
			n.Body = "Livraison en cours"
		default:
			return Notification{}, false
		}
	case events.PartialStarted:
		n.Body = progressLabel("Transcription", event.Partial, event.Total)
	case events.PartialDone:
		n.Body = progressLabel("Segment termine", event.Partial, event.Total)
	case events.PartialFailed, events.Degraded:
		n.Body = "Fonctionnement degrade"
		n.Urgency = 2
	case events.Delivered:
		n.Body = "Texte livre"
	case events.Terminal:
		n.TimeoutMS = 3000
		switch event.Result {
		case "completed":
			n.Body = "Dictee terminee"
		case "cancelled":
			n.Body = "Dictee annulee"
		case "no_speech":
			n.Body = "Aucune parole detectee"
		default:
			n.Body = "Echec de la dictee"
			n.Urgency = 2
		}
	default:
		return Notification{}, false
	}
	return n, true
}

func progressLabel(label string, partial, total int) string {
	if partial > 0 && total > 0 {
		return fmt.Sprintf("%s %d/%d", label, partial, total)
	}
	return label
}

type DBusNotificationClient struct {
	conn *dbus.Conn
	obj  dbus.BusObject
}

func NewDBusNotificationClient() (*DBusNotificationClient, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	return &DBusNotificationClient{
		conn: conn,
		obj:  conn.Object("org.freedesktop.Notifications", dbus.ObjectPath("/org/freedesktop/Notifications")),
	}, nil
}

func CheckDesktopNotifications(ctx context.Context) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Object(
		"org.freedesktop.Notifications",
		dbus.ObjectPath("/org/freedesktop/Notifications"),
	).CallWithContext(ctx, "org.freedesktop.DBus.Peer.Ping", 0).Err
}

func (c *DBusNotificationClient) Notify(ctx context.Context, replacesID uint32, n Notification) (uint32, error) {
	if c == nil || c.obj == nil {
		return 0, fmt.Errorf("client D-Bus non initialise")
	}
	var id uint32
	call := c.obj.CallWithContext(ctx, "org.freedesktop.Notifications.Notify", 0,
		"Murmur", replacesID, "audio-input-microphone-symbolic", n.Summary, n.Body,
		[]string{}, map[string]dbus.Variant{"urgency": dbus.MakeVariant(n.Urgency)}, n.TimeoutMS)
	if call.Err != nil {
		return 0, call.Err
	}
	if err := call.Store(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (c *DBusNotificationClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.conn.Close()
	return nil
}
