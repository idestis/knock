// Package hub is the relay's in-memory core: doors, knocks, sessions and
// byte forwarding for one replica. Every state change happens under one
// mutex, which gives the same guarantees the multi-replica design gets
// from conditional writes (section 8 of DESIGN.md).
package hub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"github.com/idestis/knock/protocol"
	"github.com/idestis/knock/relay/internal/auth"
	"github.com/idestis/knock/relay/internal/ratelimit"
)

type Config struct {
	AllowAnonymous bool
	KnockTimeout   time.Duration
	Signer         *auth.Signer
	Providers      []string
	RelayName      string
}

type Hub struct {
	cfg        Config
	saltSecret []byte

	mu       sync.Mutex
	doors    map[string]*door
	knocks   map[string]*knock
	clients  map[*client]struct{}
	draining bool

	doorFails    *ratelimit.Limiter // 5 wrong codes per door / 10 min
	knockerFails *ratelimit.Limiter // 10 wrong codes per knocker / 10 min
	doorTotal    *ratelimit.Limiter // 20 wrong codes forces a new code
	saltLookups  *ratelimit.Limiter
}

type door struct {
	id       string
	keyHash  []byte
	owner    protocol.Identity
	device   string
	code     protocol.CodeRecord
	host     *client
	session  *session
	watchers map[*client]struct{}
	knocks   map[string]*knock
}

type knock struct {
	id, reqID   string
	door        *door
	guest       *client
	codeVersion int64
	timer       *time.Timer
}

type session struct {
	id          string
	door        *door
	host, guest *client
	guestInput  bool
	started     time.Time
}

type frame struct {
	typ websocket.MessageType
	b   []byte
}

type client struct {
	h      *Hub
	conn   *websocket.Conn
	ip     string
	ctx    context.Context
	cancel context.CancelFunc
	out    chan frame

	ident  protocol.Identity
	device string
	kind   string

	door     *door
	session  *session
	knock    *knock
	watching map[string]*door
}

func New(cfg Config) *Hub {
	if cfg.KnockTimeout == 0 {
		cfg.KnockTimeout = 60 * time.Second
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &Hub{
		cfg: cfg, saltSecret: secret,
		doors: map[string]*door{}, knocks: map[string]*knock{}, clients: map[*client]struct{}{},
		doorFails:    ratelimit.New(5, 10*time.Minute),
		knockerFails: ratelimit.New(10, 10*time.Minute),
		doorTotal:    ratelimit.New(20, time.Hour),
		saltLookups:  ratelimit.New(60, 10*time.Minute),
	}
}

// Stats for /metrics-style reporting.
func (h *Hub) Stats() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	open, sessions := 0, 0
	for _, d := range h.doors {
		if d.host != nil {
			open++
		}
		if d.session != nil {
			sessions++
		}
	}
	return map[string]int{"clients": len(h.clients), "open_doors": open, "live_sessions": sessions, "pending_knocks": len(h.knocks)}
}

// Drain tells every client to reconnect elsewhere and stops new sockets.
func (h *Hub) Drain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.draining = true
	for c := range h.clients {
		if c.session == nil {
			c.send(protocol.TypeRelayDraining, "", nil)
		}
	}
}

// LiveSessions reports how many sessions are still running.
func (h *Hub) LiveSessions() int { return h.Stats()["live_sessions"] }

// CloseAll ends every socket.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	cs := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		cs = append(cs, c)
	}
	h.mu.Unlock()
	for _, c := range cs {
		c.conn.Close(websocket.StatusGoingAway, "relay restarting")
	}
}

// ServeHTTP upgrades /v1/ws.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	draining := h.draining
	h.mu.Unlock()
	if draining {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{h: h, conn: conn, ip: clientIP(r), ctx: ctx, cancel: cancel, out: make(chan frame, 1024), watching: map[string]*door{}}
	go c.writeLoop()
	go c.pingLoop()
	c.readLoop()
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (c *client) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case f := <-c.out:
			ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
			err := c.conn.Write(ctx, f.typ, f.b)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

func (c *client) pingLoop() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

func (c *client) readLoop() {
	defer func() {
		c.h.disconnect(c)
		c.cancel()
		c.conn.CloseNow()
	}()

	// First message must be hello.
	hctx, hcancel := context.WithTimeout(c.ctx, 15*time.Second)
	typ, data, err := c.conn.Read(hctx)
	hcancel()
	if err != nil || typ != websocket.MessageText {
		return
	}
	var env protocol.Envelope
	if json.Unmarshal(data, &env) != nil || env.Type != protocol.TypeHello {
		c.conn.Close(websocket.StatusPolicyViolation, "hello first")
		return
	}
	if !c.h.hello(c, env) {
		// give the writer a moment to flush the error
		time.Sleep(200 * time.Millisecond)
		c.conn.Close(websocket.StatusPolicyViolation, "unauthorised")
		return
	}

	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			c.h.forward(c, data)
		case websocket.MessageText:
			var env protocol.Envelope
			if json.Unmarshal(data, &env) != nil {
				c.sendRaw(protocol.EncodeError("", protocol.ErrBadRequest, "malformed message"))
				continue
			}
			c.h.handle(c, env)
		}
	}
}

// send queues a control message. Never blocks; a client that cannot keep up
// with control traffic is dropped. Safe to call with h.mu held.
func (c *client) send(typ, id string, v any) {
	b, err := protocol.Encode(typ, id, v)
	if err != nil {
		slog.Error("encode", "type", typ, "err", err)
		return
	}
	c.sendRaw(b)
}

func (c *client) sendRaw(b []byte) {
	select {
	case c.out <- frame{websocket.MessageText, b}:
	default:
		c.cancel()
	}
}

func (c *client) fail(id, code, msg string) { c.sendRaw(protocol.EncodeError(id, code, msg)) }

func (c *client) knockerKey() string {
	if c.ident.Verified {
		return c.ident.ID
	}
	return "ip:" + c.ip
}

var (
	doorIDRe = regexp.MustCompile(`^[a-z0-9]{4,32}$`)
)

func sanitize(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func newID(prefix string) string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func decode[T any](env protocol.Envelope) (T, bool) {
	var v T
	if len(env.Data) == 0 {
		return v, false
	}
	return v, json.Unmarshal(env.Data, &v) == nil
}

func (h *Hub) hello(c *client, env protocol.Envelope) bool {
	in, ok := decode[protocol.Hello](env)
	if !ok {
		c.fail(env.ID, protocol.ErrBadRequest, "bad hello")
		return false
	}
	if in.Token != "" {
		claims, err := h.cfg.Signer.Verify(in.Token, auth.KindAccess)
		if err != nil {
			c.fail(env.ID, protocol.ErrUnauthorised, "Sign-in expired.")
			return false
		}
		c.ident = claims.Identity()
	} else {
		if !h.cfg.AllowAnonymous {
			c.fail(env.ID, protocol.ErrAnonForbidden, "This relay needs you to sign in.")
			return false
		}
		name := sanitize(in.Name, 40)
		if name == "" {
			name = "Guest"
		}
		c.ident = protocol.Identity{ID: newID("anon:"), Name: name, Provider: "anonymous", Verified: false}
	}
	c.device = sanitize(in.Device, 64)
	if c.device == "" {
		c.device = "unknown"
	}
	c.kind = sanitize(in.Client, 16)

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	c.send(protocol.TypeWelcome, env.ID, protocol.Welcome{
		User: c.ident, Relay: h.cfg.RelayName, Protocol: protocol.Version,
		KnockTimeout: int(h.cfg.KnockTimeout / time.Second), AllowsAnon: h.cfg.AllowAnonymous, AuthProviders: h.cfg.Providers,
	})
	slog.Info("hello", "user", c.ident.ID, "provider", c.ident.Provider, "client", c.kind)
	return true
}

func (h *Hub) handle(c *client, env protocol.Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch env.Type {
	case protocol.TypeDoorOpen:
		h.doorOpen(c, env)
	case protocol.TypeDoorCodeSet:
		h.doorCodeSet(c, env)
	case protocol.TypeDoorClose:
		h.doorClose(c, env)
	case protocol.TypeDoorSaltGet:
		h.saltGet(c, env)
	case protocol.TypeRecentCheck:
		h.recentCheck(c, env)
	case protocol.TypeKnockRequest:
		h.knockRequest(c, env)
	case protocol.TypeKnockAnswer:
		h.knockAnswer(c, env)
	case protocol.TypeKnockCancel:
		if c.knock != nil {
			h.cancelKnock(c.knock, "", "cancelled")
		}
		c.send(protocol.TypeOK, env.ID, nil)
	case protocol.TypeSessionEnd:
		if c.session != nil {
			reason := "guest_left"
			if c.session.host == c {
				reason = "host_ended"
			}
			h.endSession(c.session, reason)
		}
		c.send(protocol.TypeOK, env.ID, nil)
	case protocol.TypeSessionResize:
		in, ok := decode[protocol.SessionResize](env)
		if ok && c.session != nil && c.session.host == c && in.Cols > 0 && in.Rows > 0 && in.Cols <= 1000 && in.Rows <= 1000 {
			c.session.guest.send(protocol.TypeSessionResize, "", in)
		}
	case protocol.TypeSessionMode:
		in, ok := decode[protocol.SessionMode](env)
		if ok && c.session != nil && c.session.host == c {
			c.session.guestInput = in.GuestInput
			c.session.guest.send(protocol.TypeSessionMode, "", in)
		}
	default:
		c.fail(env.ID, protocol.ErrUnsupported, "unknown message "+env.Type)
	}
}

func validCode(r protocol.CodeRecord) bool {
	return r.KDF == protocol.KDFPBKDF2SHA256 && len(r.Salt) == protocol.SaltLen && len(r.CodeHash) == protocol.HashLen &&
		r.Iterations >= 100_000 && r.Iterations <= 10_000_000
}

func (d *door) presence() string {
	switch {
	case d.host == nil:
		return protocol.PresenceOffline
	case d.session != nil:
		return protocol.PresenceBusy
	default:
		return protocol.PresenceOnline
	}
}

func (d *door) entry() protocol.RecentEntry {
	o := d.owner
	return protocol.RecentEntry{DoorID: d.id, Status: d.presence(), Owner: &o, Device: d.device, CodeVersion: d.code.CodeVersion}
}

func (h *Hub) notifyPresence(d *door) {
	e := d.entry()
	for w := range d.watchers {
		w.send(protocol.TypePresenceUpdate, "", e)
	}
}

func (h *Hub) codeChanged(d *door) {
	if d.session != nil {
		h.endSession(d.session, "code_changed")
	}
	for w := range d.watchers {
		w.send(protocol.TypeDoorCodeChanged, "", protocol.DoorRef{DoorID: d.id})
		delete(w.watching, d.id)
	}
	d.watchers = map[*client]struct{}{}
	for _, k := range d.knocks {
		h.cancelKnock(k, protocol.ErrCodeMismatch, "code_changed")
	}
	h.doorTotal.Reset(d.id)
	h.doorFails.Reset(d.id)
}

func (h *Hub) doorOpen(c *client, env protocol.Envelope) {
	in, ok := decode[protocol.DoorOpen](env)
	if !ok || !doorIDRe.MatchString(in.DoorID) || len(in.DoorKey) < 16 || !validCode(in.Code) {
		c.fail(env.ID, protocol.ErrBadRequest, "bad door")
		return
	}
	if c.session != nil && c.session.guest == c {
		h.endSession(c.session, "guest_left")
	}
	keyHash := sha256.Sum256([]byte(in.DoorKey))
	d, exists := h.doors[in.DoorID]
	if exists && !hmac.Equal(d.keyHash, keyHash[:]) {
		c.fail(env.ID, protocol.ErrDoorTaken, "That door belongs to someone else.")
		return
	}
	if c.door != nil && c.door != d {
		h.shutDoor(c.door, "door_closed")
	}
	if !exists {
		d = &door{id: in.DoorID, keyHash: keyHash[:], watchers: map[*client]struct{}{}, knocks: map[string]*knock{}}
		h.doors[d.id] = d
	}
	if d.host != nil && d.host != c {
		// Opened again from a newer socket (reconnect): the old one loses it.
		old := d.host
		h.shutDoor(d, "opened_elsewhere")
		old.door = nil
		old.send(protocol.TypeDoorClose, "", protocol.SessionEnd{Reason: "opened_elsewhere"})
	}
	if exists && (d.code.CodeVersion != in.Code.CodeVersion || !hmac.Equal(d.code.CodeHash, in.Code.CodeHash)) {
		h.codeChanged(d)
	}
	d.code = in.Code
	d.owner = c.ident
	d.device = c.device
	d.host = c
	c.door = d
	c.send(protocol.TypeDoorOpened, env.ID, protocol.DoorOpened{DoorID: d.id})
	h.notifyPresence(d)
	slog.Info("door open", "door", d.id, "user", c.ident.ID)
}

// shutDoor takes the door offline; it stays known so Recent can show it.
func (h *Hub) shutDoor(d *door, reason string) {
	for _, k := range d.knocks {
		h.cancelKnock(k, protocol.ErrDoorOffline, reason)
	}
	if d.session != nil {
		h.endSession(d.session, reason)
	}
	if d.host != nil {
		d.host.door = nil
	}
	d.host = nil
	h.notifyPresence(d)
}

func (h *Hub) doorCodeSet(c *client, env protocol.Envelope) {
	in, ok := decode[protocol.DoorCodeSet](env)
	if !ok || !validCode(in.Code) || c.door == nil {
		c.fail(env.ID, protocol.ErrBadRequest, "no open door or bad code")
		return
	}
	d := c.door
	d.code = in.Code
	h.codeChanged(d)
	c.send(protocol.TypeOK, env.ID, nil)
	slog.Info("door new code", "door", d.id, "version", in.Code.CodeVersion)
}

func (h *Hub) doorClose(c *client, env protocol.Envelope) {
	if c.door != nil {
		slog.Info("door closed", "door", c.door.id)
		h.shutDoor(c.door, "door_closed")
	}
	c.send(protocol.TypeOK, env.ID, nil)
}

func (h *Hub) saltGet(c *client, env protocol.Envelope) {
	in, ok := decode[protocol.DoorSaltGet](env)
	if !ok || !doorIDRe.MatchString(in.DoorID) {
		c.fail(env.ID, protocol.ErrBadRequest, "bad door id")
		return
	}
	if h.saltLookups.Add(c.knockerKey()) > 60 {
		c.fail(env.ID, protocol.ErrRateLimited, "Too many attempts. Wait a few minutes.")
		return
	}
	out := protocol.DoorSalt{DoorID: in.DoorID, KDF: protocol.KDFPBKDF2SHA256, Iterations: protocol.PBKDF2Iterations}
	if d, ok := h.doors[in.DoorID]; ok {
		out.Salt, out.KDF, out.Iterations = d.code.Salt, d.code.KDF, d.code.Iterations
	} else {
		// Unknown doors get a stable fake salt so lookups do not reveal which doors exist.
		m := hmac.New(sha256.New, h.saltSecret)
		m.Write([]byte(in.DoorID))
		out.Salt = m.Sum(nil)[:protocol.SaltLen]
	}
	c.send(protocol.TypeDoorSalt, env.ID, out)
}

func (h *Hub) recentCheck(c *client, env protocol.Envelope) {
	in, ok := decode[protocol.RecentCheck](env)
	if !ok || len(in.Items) > 200 {
		c.fail(env.ID, protocol.ErrBadRequest, "bad recent.check")
		return
	}
	out := protocol.RecentStatus{Items: make([]protocol.RecentEntry, 0, len(in.Items))}
	for _, it := range in.Items {
		d, ok := h.doors[it.DoorID]
		if !ok || !protocol.EqualHash(d.code.CodeHash, it.CodeHash) {
			if ok {
				delete(d.watchers, c)
			}
			delete(c.watching, it.DoorID)
			// Unknown after a relay restart looks the same as offline to a
			// correct-hash holder; we cannot tell, so say offline only when
			// the door is unknown, and code_changed when the hash is wrong.
			status := protocol.PresenceCodeChanged
			if !ok {
				status = protocol.PresenceOffline
			}
			out.Items = append(out.Items, protocol.RecentEntry{DoorID: it.DoorID, Status: status})
			continue
		}
		d.watchers[c] = struct{}{}
		c.watching[d.id] = d
		out.Items = append(out.Items, d.entry())
	}
	c.send(protocol.TypeRecentStatus, env.ID, out)
}

func (h *Hub) knockRequest(c *client, env protocol.Envelope) {
	in, ok := decode[protocol.KnockRequest](env)
	if !ok || !doorIDRe.MatchString(in.DoorID) {
		c.fail(env.ID, protocol.ErrBadRequest, "bad knock")
		return
	}
	if c.session != nil {
		c.fail(env.ID, protocol.ErrBadRequest, "You are already in a session.")
		return
	}
	if c.knock != nil {
		h.cancelKnock(c.knock, "", "replaced")
	}
	kk := c.knockerKey()
	if h.knockerFails.Blocked(kk) || h.doorFails.Blocked(in.DoorID) {
		c.fail(env.ID, protocol.ErrRateLimited, "Too many wrong codes. Wait ten minutes and try again.")
		return
	}
	d, ok := h.doors[in.DoorID]
	if !ok || !protocol.EqualHash(d.code.CodeHash, in.CodeHash) {
		h.knockerFails.Add(kk)
		h.doorFails.Add(in.DoorID)
		if ok && h.doorTotal.Add(d.id) == 20 && d.host != nil {
			d.host.send(protocol.TypeDoorForceNew, "", protocol.DoorForceNewCode{Reason: "Someone tried 20 wrong codes on your door."})
		}
		slog.Info("knock", "door", in.DoorID, "from", c.ident.ID, "outcome", "code_mismatch")
		c.fail(env.ID, protocol.ErrCodeMismatch, "That code doesn't work. Ask for a new one.")
		return
	}
	d.watchers[c] = struct{}{}
	c.watching[d.id] = d
	switch {
	case d.host == nil:
		c.fail(env.ID, protocol.ErrDoorOffline, "They're not open to knocks right now.")
		return
	case d.host == c:
		c.fail(env.ID, protocol.ErrBadRequest, "That's your own door.")
		return
	case d.session != nil:
		c.fail(env.ID, protocol.ErrDoorBusy, "They're in a session.")
		return
	}
	k := &knock{id: newID("k_"), reqID: env.ID, door: d, guest: c, codeVersion: d.code.CodeVersion}
	k.timer = time.AfterFunc(h.cfg.KnockTimeout, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.knocks[k.id] == k {
			h.cancelKnock(k, protocol.ErrTimeout, "timeout")
		}
	})
	h.knocks[k.id] = k
	d.knocks[k.id] = k
	c.knock = k
	secs := int(h.cfg.KnockTimeout / time.Second)
	c.send(protocol.TypeKnockPending, env.ID, protocol.KnockPending{KnockID: k.id, ExpiresIn: secs})
	d.host.send(protocol.TypeKnockIncoming, "", protocol.KnockIncoming{KnockID: k.id, From: c.ident, Device: c.device, ExpiresIn: secs})
	slog.Info("knock", "door", d.id, "from", c.ident.ID, "outcome", "delivered")
}

// cancelKnock removes a pending knock. errCode, if set, is sent to the guest.
func (h *Hub) cancelKnock(k *knock, errCode, reason string) {
	k.timer.Stop()
	delete(h.knocks, k.id)
	delete(k.door.knocks, k.id)
	if k.guest.knock == k {
		k.guest.knock = nil
	}
	if errCode != "" {
		k.guest.fail(k.reqID, errCode, reasonText(errCode))
	}
	if k.door.host != nil {
		k.door.host.send(protocol.TypeKnockCancelled, "", protocol.KnockRef{KnockID: k.id, Reason: reason})
	}
}

func reasonText(code string) string {
	switch code {
	case protocol.ErrTimeout:
		return "No answer. Try again in a bit."
	case protocol.ErrDenied:
		return "Not now. They didn't let you in."
	case protocol.ErrDoorOffline:
		return "They closed the door."
	case protocol.ErrDoorBusy:
		return "Someone else got in first."
	case protocol.ErrCodeMismatch:
		return "The code changed. Ask for a new one."
	}
	return ""
}

func (h *Hub) knockAnswer(c *client, env protocol.Envelope) {
	in, ok := decode[protocol.KnockAnswer](env)
	k := h.knocks[in.KnockID]
	if !ok || k == nil || k.door.host != c {
		c.fail(env.ID, protocol.ErrNotFound, "That knock has gone.")
		return
	}
	d, g := k.door, k.guest
	if !in.Allow {
		k.timer.Stop()
		delete(h.knocks, k.id)
		delete(d.knocks, k.id)
		g.knock = nil
		g.fail(k.reqID, protocol.ErrDenied, reasonText(protocol.ErrDenied))
		c.send(protocol.TypeOK, env.ID, nil)
		slog.Info("knock answered", "door", d.id, "from", g.ident.ID, "outcome", "denied")
		return
	}
	// The equivalent of the conditional transaction: code unchanged, no session yet.
	if k.codeVersion != d.code.CodeVersion {
		h.cancelKnock(k, protocol.ErrCodeMismatch, "code_changed")
		c.fail(env.ID, protocol.ErrCodeMismatch, "The code changed after they knocked.")
		return
	}
	if d.session != nil || c.session != nil {
		h.cancelKnock(k, protocol.ErrDoorBusy, "busy")
		c.fail(env.ID, protocol.ErrDoorBusy, "You're already in a session.")
		return
	}
	k.timer.Stop()
	delete(h.knocks, k.id)
	delete(d.knocks, k.id)
	g.knock = nil

	s := &session{id: newID("s_"), door: d, host: c, guest: g, guestInput: true, started: time.Now()}
	d.session, c.session, g.session = s, s, s
	for _, other := range d.knocks {
		h.cancelKnock(other, protocol.ErrDoorBusy, "busy")
	}
	c.send(protocol.TypeOK, env.ID, nil)
	c.send(protocol.TypeSessionStart, "", protocol.SessionStart{SessionID: s.id, Role: protocol.RoleHost, Peer: g.ident,
		PeerDevice: g.device, DoorID: d.id, CodeVersion: d.code.CodeVersion, GuestInput: true})
	g.send(protocol.TypeSessionStart, k.reqID, protocol.SessionStart{SessionID: s.id, Role: protocol.RoleGuest, Peer: d.owner,
		PeerDevice: d.device, DoorID: d.id, CodeVersion: d.code.CodeVersion, GuestInput: true})
	h.notifyPresence(d)
	slog.Info("session start", "session", s.id, "door", d.id, "guest", g.ident.ID)
}

func (h *Hub) endSession(s *session, reason string) {
	if s.door.session == s {
		s.door.session = nil
	}
	if s.host.session == s {
		s.host.session = nil
	}
	if s.guest.session == s {
		s.guest.session = nil
	}
	end := protocol.SessionEnd{SessionID: s.id, Reason: reason}
	s.host.send(protocol.TypeSessionEnd, "", end)
	s.guest.send(protocol.TypeSessionEnd, "", end)
	h.notifyPresence(s.door)
	slog.Info("session end", "session", s.id, "door", s.door.id, "reason", reason, "duration", time.Since(s.started).Round(time.Second).String())
}

func (h *Hub) disconnect(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	if c.knock != nil {
		h.cancelKnock(c.knock, "", "cancelled")
	}
	if c.session != nil {
		reason := "guest_left"
		if c.session.host == c {
			reason = "host_left"
		}
		h.endSession(c.session, reason)
	}
	if c.door != nil && c.door.host == c {
		h.shutDoor(c.door, "host_left")
	}
	for _, d := range c.watching {
		delete(d.watchers, c)
	}
}

// forward moves terminal bytes between the two ends of a session.
func (h *Hub) forward(c *client, b []byte) {
	h.mu.Lock()
	s := c.session
	var to *client
	if s != nil {
		if s.host == c {
			to = s.guest
		} else if s.guest == c && s.guestInput {
			to = s.host
		}
	}
	h.mu.Unlock()
	if to == nil {
		return
	}
	select {
	case to.out <- frame{websocket.MessageBinary, b}:
	case <-to.ctx.Done():
	case <-c.ctx.Done():
	case <-time.After(15 * time.Second):
		to.cancel() // peer stopped reading
	}
}
