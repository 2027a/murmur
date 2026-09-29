package ui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/logging"
)

const (
	notificationCallTimeout  = 2 * time.Second
	notificationCloseTimeout = notificationCallTimeout + 250*time.Millisecond
)

type notificationRequest struct {
	value    Notification
	terminal bool
}

type Notification struct {
	Summary   string
	Body      string
	Category  string
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
	wake   chan struct{}
	done   chan struct{}
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc

	callTimeout  time.Duration
	closeTimeout time.Duration
	mu           sync.Mutex
	pending      *notificationRequest
	closed       bool
	terminalSeen bool
	activeCancel context.CancelFunc
	activeTerm   bool
}

func NewNotify(client NotificationClient, warn WarnFunc) *Notify {
	return newNotify(client, warn, notificationCallTimeout, notificationCloseTimeout)
}

func newNotify(client NotificationClient, warn WarnFunc, callTimeout, closeTimeout time.Duration) *Notify {
	ctx, cancel := context.WithCancel(context.Background())
	u := &Notify{
		client:       client,
		warn:         warn,
		wake:         make(chan struct{}, 1),
		done:         make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
		callTimeout:  callTimeout,
		closeTimeout: closeTimeout,
	}
	go u.run()
	return u
}

// NewDesktopNotifications degrades to the no-op UI when the session bus is absent.
func NewDesktopNotifications(warn WarnFunc) UI {
	client, err := NewDBusNotificationClient()
	if err != nil {
		warning(logging.ComponentDBus, warn, "notifications D-Bus indisponibles")
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
	request := notificationRequest{value: notification, terminal: event.Type == events.Terminal}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	if u.terminalSeen && !request.terminal {
		u.mu.Unlock()
		return
	}
	if request.terminal {
		u.terminalSeen = true
	}
	if u.pending == nil || request.terminal || !u.pending.terminal {
		u.pending = &request
	}
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *Notify) Close() {
	if u == nil {
		return
	}
	u.once.Do(func() {
		u.mu.Lock()
		u.closed = true
		if !u.terminalSeen {
			u.pending = nil
		}
		if u.activeCancel != nil && (!u.activeTerm || !u.terminalSeen) {
			u.activeCancel()
		}
		u.mu.Unlock()
		select {
		case u.wake <- struct{}{}:
		default:
		}

		timer := time.NewTimer(u.closeTimeout)
		select {
		case <-u.done:
		case <-timer.C:
			u.cancel()
			<-u.done
		}
		timer.Stop()
		u.cancel()
	})
}

func (u *Notify) run() {
	defer close(u.done)
	defer u.client.Close()
	var replacesID uint32
	for {
		u.mu.Lock()
		request := u.pending
		u.pending = nil
		if request == nil {
			closed := u.closed
			u.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-u.wake:
				continue
			case <-u.ctx.Done():
				return
			}
		}
		ctx, cancel := context.WithTimeout(u.ctx, u.callTimeout)
		u.activeCancel = cancel
		u.activeTerm = request.terminal
		u.mu.Unlock()

		id, err := u.client.Notify(ctx, replacesID, request.value)
		cancel()
		u.mu.Lock()
		u.activeCancel = nil
		u.activeTerm = false
		u.mu.Unlock()
		if err != nil {
			warning(logging.ComponentDBus, u.warn, "appel de notification échoué")
			continue
		}
		replacesID = id
	}
}

func notificationForEvent(event events.Event) (Notification, bool) {
	if backgroundProgress(event) {
		return Notification{}, false
	}
	n := Notification{Summary: "🟢 Murmur", Urgency: 1, TimeoutMS: 2200}
	switch event.Type {
	case events.SessionStarted:
		n.Body = "Démarrage de la capture"
		n.Category = "murmur.starting"
	case events.CaptureReady:
		n.Body = "Écoute en cours"
		n.Category = "murmur.recording"
		n.TimeoutMS = 0
	case events.StateChanged:
		switch event.State {
		case "stopping":
			n.Body = "Arrêt de la capture"
			n.Category = "murmur.stopping"
		case "transcribing":
			n.Body = "Transcription en cours"
			n.Category = "murmur.processing"
		case "processing":
			n.Body = "Traitement en cours"
			n.Category = "murmur.processing"
		case "delivering":
			n.Body = "Livraison en cours"
			n.Category = "murmur.processing"
		default:
			return Notification{}, false
		}
	case events.PartialStarted:
		n.Body = progressLabel("Transcription", event.Partial, event.Total)
		n.Category = "murmur.processing"
	case events.PartialDone:
		n.Body = progressLabel("Segment terminé", event.Partial, event.Total)
		n.Category = "murmur.processing"
	case events.PartialFailed, events.Degraded:
		n.Summary = "🔴 Murmur"
		n.Body = "Fonctionnement dégradé"
		n.Category = "murmur.warning"
		n.Urgency = 2
		n.TimeoutMS = 5000
	case events.Delivered:
		return Notification{}, false
	case events.Terminal:
		switch event.Result {
		case "completed":
			n.Body = "Dictée terminée"
			n.Category = "murmur.success"
			n.TimeoutMS = 3500
		case "cancelled":
			n.Summary = "🟠 Murmur"
			n.Body = "Dictée annulée"
			n.Category = "murmur.cancelled"
			n.TimeoutMS = 3500
		case "no_speech":
			n.Body = "Aucune parole détectée"
			n.Category = "murmur.warning"
			n.TimeoutMS = 4500
		default:
			n.Summary = "🔴 Murmur"
			n.Body = "Échec de la dictée"
			n.Category = "murmur.error"
			n.Urgency = 2
			n.TimeoutMS = 6000
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
		[]string{}, map[string]dbus.Variant{
			"urgency":  dbus.MakeVariant(n.Urgency),
			"category": dbus.MakeVariant(n.Category),
		}, n.TimeoutMS)
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
