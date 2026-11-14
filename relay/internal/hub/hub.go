// Package hub is the relay's core: doors, knocks, sessions and byte
// forwarding for one replica. Every in-memory state change happens under
// one mutex. Door registrations are also written through to a store
// (relay/internal/store), so who holds a door id survives a restart; store
// I/O happens outside the mutex, and ownership changes are conditional
// writes (section 8 of DESIGN.md).
package hub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"github.com/idestis/knock/protocol"
	"github.com/idestis/knock/relay/internal/auth"
	"github.com/idestis/knock/relay/internal/ratelimit"
	"github.com/idestis/knock/relay/internal/store"
)

type Config struct {
	AllowAnonymous bool
	KnockTimeout   time.Duration
	Signer         *auth.Signer
	Providers      []string
	RelayName      string
	// TrustedProxies are the proxies whose X-Forwarded-For the relay
	// believes. Empty: the relay uses the socket's address and ignores the
	// header, since anyone can set it.
	TrustedProxies []netip.Prefix
	// DoorTTL is how long a closed door with nobody watching stays known
	// before the relay forgets it. Zero means 24 hours. A store should
	// expire records after the same time.
	DoorTTL time.Duration
	// Store keeps door registrations across restarts. Nil means an
	// in-memory store with DoorTTL.
	Store store.Store
	// Heartbeat is how often open doors refresh their store TTL. Zero
	// means a minute.
	Heartbeat time.Duration
	// ClaimMaxAge is how long a door claim stays good after the relay
	// issued it. Every door.open issues a fresh one. Zero means 90 days.
	ClaimMaxAge time.Duration
}

// Limits. Wrong codes are counted per knocker and per door (section 5 of
// DESIGN.md); the rest keep one client from flooding a host or the relay.
const (
	maxDoors        = 50_000  // doors the relay keeps in memory
	maxQueuedBytes  = 4 << 20 // session bytes queued for one client
	prevHashes      = 4       // earlier code hashes a door remembers
	doorForceNewAt  = 20      // wrong codes on a door before the host gets a new one
	doorHardLimit   = 30      // wrong codes on a door in an hour before it refuses everyone
	knocksPerDoor   = 12      // knock.request per knocker per door per 10 minutes
	recentChecks    = 60      // recent.check per knocker per 10 minutes
	doorOpensLimit  = 60      // door.open per knocker per 10 minutes
	doorCreateLimit = 20      // new doors per knocker per hour
	storeTimeout    = 3 * time.Second
	// reasonReclaimed: the door's rightful key holder took the id back with
	// a signed claim. Sent to the holder that lost it, and ends its session.
	reasonReclaimed = "door_reclaimed"
	storeDownText   = "The relay can't check doors right now. Try again in a minute."
)

type Hub struct {
	cfg        Config
	saltSecret []byte
	store      store.Store
	claims     claims
	opening    idLocks // door.open, one per door id at a time

	mu       sync.Mutex
	doors    map[string]*door
	knocks   map[string]*knock
	clients  map[*client]struct{}
	draining bool
	dirty    map[string]struct{} // doors to write through to the store

	doorFails    *ratelimit.Limiter // 5 wrong codes per door / 10 min, per signed-in knocker or shared by anonymous ones
	knockerFails *ratelimit.Limiter // 10 wrong codes per knocker / 10 min
	doorTotal    *ratelimit.Limiter // 20 wrong codes forces a new code, 30 shuts the door to knocks for the hour
	seenFails    *ratelimit.Limiter // a wrong hash a knocker repeats counts once
	saltLookups  *ratelimit.Limiter
	knockSends   *ratelimit.Limiter
	recentLimit  *ratelimit.Limiter
	doorOpens    *ratelimit.Limiter
	doorCreates  *ratelimit.Limiter
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
	prev     [][]byte  // hashes of earlier codes, newest last
	closedAt time.Time // when the host left; the door is forgotten DoorTTL later
	// claimedAt is when this key first held the id (unix ms); an
	// older signed claim beats it.
	claimedAt int64
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
	h       *Hub
	conn    *websocket.Conn
	ip      string
	ctx     context.Context
	cancel  context.CancelFunc
	out     chan frame
	queued  atomic.Int64  // session bytes waiting in out
	drained chan struct{} // poked when session bytes leave out

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
	if cfg.DoorTTL == 0 {
		cfg.DoorTTL = 24 * time.Hour
	}
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = time.Minute
	}
	if cfg.Store == nil {
		cfg.Store = store.NewMemory(cfg.DoorTTL)
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	// Claims are keyed from the token secret, so they last as long as it
	// does. Without a signer (tests) they last as long as the process.
	claimKey := make([]byte, 32)
	_, _ = rand.Read(claimKey)
	if cfg.Signer != nil {
		claimKey = cfg.Signer.Derive("door-claim")
	}
	h := &Hub{
		cfg: cfg, saltSecret: secret, store: cfg.Store, claims: claims{key: claimKey, maxAge: cfg.ClaimMaxAge},
		doors: map[string]*door{}, knocks: map[string]*knock{}, clients: map[*client]struct{}{}, dirty: map[string]struct{}{},
		doorFails:    ratelimit.New(5, 10*time.Minute),
		knockerFails: ratelimit.New(10, 10*time.Minute),
		doorTotal:    ratelimit.New(doorHardLimit, time.Hour),
		seenFails:    ratelimit.New(1, 10*time.Minute),
		saltLookups:  ratelimit.New(60, 10*time.Minute),
		knockSends:   ratelimit.New(knocksPerDoor, 10*time.Minute),
		recentLimit:  ratelimit.New(recentChecks, 10*time.Minute),
		doorOpens:    ratelimit.New(doorOpensLimit, 10*time.Minute),
		doorCreates:  ratelimit.New(doorCreateLimit, time.Hour),
	}
	go h.pruneLoop()
	go h.heartbeatLoop()
	return h
}

// pruneLoop forgets doors that have been closed for DoorTTL with nobody
// watching or knocking, so the door map can't grow without bound. A
// forgotten door looks the same as one that never existed.
func (h *Hub) pruneLoop() {
	every := min(h.cfg.DoorTTL/2, time.Minute)
	for range time.Tick(every) {
		h.prune(time.Now())
	}
}

func (h *Hub) prune(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, d := range h.doors {
		if d.host == nil && d.session == nil && len(d.watchers) == 0 && len(d.knocks) == 0 && now.Sub(d.closedAt) >= h.cfg.DoorTTL {
			delete(h.doors, id)
		}
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
	return map[string]int{"clients": len(h.clients), "doors": len(h.doors), "open_doors": open, "live_sessions": sessions, "pending_knocks": len(h.knocks)}
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
	c := &client{h: h, conn: conn, ip: h.clientIP(r), ctx: ctx, cancel: cancel, out: make(chan frame, 1024),
		drained: make(chan struct{}, 1), watching: map[string]*door{}}
	go c.writeLoop()
	go c.pingLoop()
	c.readLoop()
}

// clientIP is the address anonymous limits are keyed on. X-Forwarded-For
// counts only when the socket comes from a trusted proxy, and then the
// relay walks it from the right and takes the first hop it doesn't trust:
// everything left of that was written by the client and can be anything.
// ClientIP is the address limits key on for r: the socket's address, or the
// first untrusted X-Forwarded-For hop when the socket is a trusted proxy.
func (h *Hub) ClientIP(r *http.Request) string { return h.clientIP(r) }

func (h *Hub) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !h.trusted(addr) {
		return host
	}
	addr = addr.Unmap()
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := parseHop(hops[i])
		if err != nil {
			break // garbage from here on: settle for the last hop we could read
		}
		addr = hop
		if !h.trusted(hop) {
			break
		}
	}
	return addr.String()
}

func parseHop(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), nil
	}
	a, err := netip.ParseAddr(s)
	return a.Unmap(), err
}

func (h *Hub) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range h.cfg.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ParseTrustedProxies reads KNOCK_TRUSTED_PROXIES: comma-separated CIDRs or
// single addresses. Empty means trust no proxy.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			if p.Addr().Is4In6() {
				return nil, fmt.Errorf("KNOCK_TRUSTED_PROXIES: write %q as an IPv4 CIDR", f)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("KNOCK_TRUSTED_PROXIES: %q is not a CIDR or an address", f)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
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
			c.sent(f)
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

// sent releases a session frame's bytes from the client's budget.
func (c *client) sent(f frame) {
	if f.typ != websocket.MessageBinary {
		return
	}
	c.queued.Add(-int64(len(f.b)))
	select {
	case c.drained <- struct{}{}:
	default:
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

// failStale refuses a door.open or door.code.set whose code is older than
// the door's, and says which version the relay holds, so the key holder can
// make a fresh code above it. Only code_stale ever carries a version.
func (c *client) failStale(id string, held int64) {
	c.sendRaw(protocol.EncodeCodeStale(id, "Your door has a newer code than this one. Reload it and try again.", int(held)))
}

// congested reports a client that isn't keeping up. Knock notices to a
// congested host are shed or refused rather than queued, so a knocker can
// never fill the host's queue and get it dropped.
func (c *client) congested() bool { return len(c.out) >= cap(c.out)/2 }

// sendSoft queues a notice the client can live without, unless it's congested.
func (c *client) sendSoft(typ, id string, v any) {
	if !c.congested() {
		c.send(typ, id, v)
	}
}

// failKey scopes a door's wrong-code budget. Signed-in knockers each get
// their own, so one person's guesses can't lock out another who has the
// code; anonymous knockers can't be told apart, so they share one.
func (c *client) failKey(doorID string) string {
	if c.ident.Verified {
		return doorID + "|" + c.ident.ID
	}
	return doorID + "|anon"
}

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
	if env.Type == protocol.TypeDoorOpen {
		h.doorOpen(c, env) // does its own locking around store I/O
		h.flush()
		return
	}
	failed := h.preload(c, env)
	h.mu.Lock()
	defer h.flush() // runs after the unlock below
	defer h.mu.Unlock()
	switch env.Type {
	case protocol.TypeDoorCodeSet:
		h.doorCodeSet(c, env)
	case protocol.TypeDoorClose:
		h.doorClose(c, env)
	case protocol.TypeDoorSaltGet:
		h.saltGet(c, env, failed)
	case protocol.TypeRecentCheck:
		h.recentCheck(c, env, failed)
	case protocol.TypeKnockRequest:
		h.knockRequest(c, env, failed)
	case protocol.TypeKnockAnswer:
		h.knockAnswer(c, env)
	case protocol.TypeKnockCancel:
		if c.knock != nil {
			h.cancelKnock(c.knock, "", "cancelled")
		}
		c.send(protocol.TypeOK, env.ID, nil)
	case protocol.TypeSessionEnd:
		// A session_id names the session to end; one that isn't the
		// caller's current session (a late end for an earlier one) is
		// ignored rather than ending the wrong session.
		in, _ := decode[protocol.SessionEnd](env)
		if s := c.session; s != nil && (in.SessionID == "" || in.SessionID == s.id) {
			h.endSession(s, endReason(s.host == c, in.Reason))
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
	// The shared anonymous budget starts over with the code; per-person
	// budgets run out on their own.
	h.doorFails.Reset(d.id + "|anon")
}

// setCode swaps in a new code record and cuts off everyone who had the old
// one. The old hash is kept so Recent can say "code changed" to people who
// held it, without that answer telling anyone else the door exists.
func (h *Hub) setCode(d *door, rec protocol.CodeRecord) {
	if len(d.code.CodeHash) > 0 {
		d.prev = append(d.prev, d.code.CodeHash)
		if len(d.prev) > prevHashes {
			d.prev = d.prev[len(d.prev)-prevHashes:]
		}
	}
	d.code = rec
	h.markDirty(d)
	h.codeChanged(d)
}

// staleCode reports a record older than the door's: a lower code_version,
// or the same version with a different hash. Accepting it would bring a
// code the host already replaced back to life.
func staleCode(cur, in protocol.CodeRecord) bool {
	return in.CodeVersion < cur.CodeVersion || (in.CodeVersion == cur.CodeVersion && !protocol.EqualHash(cur.CodeHash, in.CodeHash))
}

func (d *door) wasCode(hash []byte) bool {
	for _, p := range d.prev {
		if protocol.EqualHash(p, hash) {
			return true
		}
	}
	return false
}

// doorOpen registers or reopens a door. Who holds the id is decided from
// memory, or from the store for an id memory doesn't know (after a
// restart). A new holder is written to the store with a conditional write
// before anyone sees it; a store that can't answer fails the open.
func (h *Hub) doorOpen(c *client, env protocol.Envelope) {
	in, ok := decode[protocol.DoorOpen](env)
	if !ok || !doorIDRe.MatchString(in.DoorID) || len(in.DoorKey) < 16 || !validCode(in.Code) {
		c.fail(env.ID, protocol.ErrBadRequest, "bad door")
		return
	}
	kk := c.knockerKey()
	if h.doorOpens.Add(kk) > doorOpensLimit {
		c.fail(env.ID, protocol.ErrRateLimited, "Too many tries. Wait a few minutes.")
		return
	}
	sum := sha256.Sum256([]byte(in.DoorKey))
	keyHash := sum[:]
	claimSince, claimOK := h.claims.verify(in.Claim, in.DoorID, keyHash)

	// Only door.open changes which key holds an id, one per id at a time,
	// so what we read below stays true until we write.
	defer h.opening.lock(in.DoorID)()

	h.mu.Lock()
	var cur *store.Door
	if d, ok := h.doors[in.DoorID]; ok {
		r := d.record()
		cur = &r
	}
	h.mu.Unlock()
	if cur == nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		got, err := h.store.Load(ctx, []string{in.DoorID})
		cancel()
		if err != nil {
			slog.Warn("store load failed", "door", in.DoorID, "err", err)
			c.fail(env.ID, protocol.ErrRateLimited, storeDownText)
			return
		}
		if r, ok := got[in.DoorID]; ok {
			cur = &r
		}
	}

	takeover := false
	switch {
	case cur == nil:
	case hmac.Equal(cur.KeyHash, keyHash):
		if staleCode(cur.Code, in.Code) {
			c.failStale(env.ID, cur.Code.CodeVersion)
			return
		}
	case claimOK && claimSince < cur.ClaimedAt:
		// The presenter held this id before the current holder did.
		takeover = true
	default:
		c.fail(env.ID, protocol.ErrDoorTaken, "That door belongs to someone else.")
		return
	}

	now := time.Now()
	since := now.UnixMilli()
	if cur != nil && !takeover {
		since = cur.ClaimedAt
	}
	if claimOK && claimSince < since {
		since = claimSince
	}
	if cur == nil || takeover {
		if cur == nil {
			h.mu.Lock()
			full := len(h.doors) >= maxDoors
			h.mu.Unlock()
			if full {
				c.fail(env.ID, protocol.ErrRateLimited, "This relay is full. Try again later.")
				return
			}
			if h.doorCreates.Add(kk) > doorCreateLimit {
				c.fail(env.ID, protocol.ErrRateLimited, "Too many new doors. Wait a while and try again.")
				return
			}
		}
		holder := keyHash
		if takeover {
			holder = cur.KeyHash
		}
		rec := store.Door{ID: in.DoorID, KeyHash: keyHash, Owner: c.ident, Device: c.device, Code: in.Code, ClaimedAt: since, LastSeen: now}
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		err := h.store.Save(ctx, rec, holder)
		cancel()
		switch {
		case errors.Is(err, store.ErrTaken):
			// Another relay took it between our read and write.
			c.fail(env.ID, protocol.ErrDoorTaken, "That door belongs to someone else.")
			return
		case errors.Is(err, store.ErrStale):
			// Only the same key gets ErrStale; tell it the version the store holds.
			var held int64
			lctx, lcancel := context.WithTimeout(context.Background(), storeTimeout)
			if got, err := h.store.Load(lctx, []string{in.DoorID}); err == nil {
				held = got[in.DoorID].Code.CodeVersion
			}
			lcancel()
			c.failStale(env.ID, held)
			return
		case err != nil:
			slog.Warn("store save failed", "door", in.DoorID, "err", err)
			c.fail(env.ID, protocol.ErrRateLimited, storeDownText)
			return
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	d, exists := h.doors[in.DoorID]
	if exists && !hmac.Equal(d.keyHash, keyHash) {
		if !takeover || !hmac.Equal(d.keyHash, cur.KeyHash) {
			c.fail(env.ID, protocol.ErrDoorTaken, "That door belongs to someone else.")
			return
		}
		slog.Info("door reclaimed", "door", d.id, "user", c.ident.ID)
		h.evict(d)
		d, exists = nil, false
	}
	if exists && staleCode(d.code, in.Code) {
		c.failStale(env.ID, d.code.CodeVersion)
		return
	}
	if c.session != nil && c.session.guest == c {
		h.endSession(c.session, "guest_left")
	}
	if c.knock != nil && (!exists || d.host != c) {
		// A host can't also be waiting at someone else's door.
		h.cancelKnockText(c.knock, protocol.ErrDoorBusy, "You opened your door, so your knock was cancelled.", "cancelled")
	}
	if c.door != nil && c.door != d {
		h.shutDoor(c.door, "door_closed")
	}
	if !exists {
		d = &door{id: in.DoorID, keyHash: keyHash, watchers: map[*client]struct{}{}, knocks: map[string]*knock{}, claimedAt: since, code: in.Code}
		if cur != nil && !takeover {
			// Known to the store from before a restart: carry its code on,
			// so a newer one below cuts off the old one's holders.
			d.code, d.prev = cur.Code, cur.Prev
		}
		h.doors[d.id] = d
	}
	d.claimedAt = min(d.claimedAt, since)
	if d.host != nil && d.host != c {
		// Opened again from a newer socket (reconnect): the old one loses it.
		old := d.host
		h.shutDoor(d, "opened_elsewhere")
		old.door = nil
		old.send(protocol.TypeDoorClose, "", protocol.SessionEnd{Reason: "opened_elsewhere"})
	}
	if in.Code.CodeVersion != d.code.CodeVersion {
		// staleCode above means this is a newer code.
		h.setCode(d, in.Code)
	}
	d.owner = c.ident
	d.device = c.device
	d.host = c
	c.door = d
	h.markDirty(d)
	c.send(protocol.TypeDoorOpened, env.ID, protocol.DoorOpened{DoorID: d.id, Claim: h.claims.issue(d.id, d.keyHash, d.claimedAt)})
	h.notifyPresence(d)
	slog.Info("door open", "door", d.id, "user", c.ident.ID)
}

// evict cuts off a door's holder after its rightful owner took the id back
// with a claim: the session ends, knocks are cancelled, watchers are told
// the code changed, and the holder gets door.close. The door is forgotten.
func (h *Hub) evict(d *door) {
	if d.session != nil {
		h.endSession(d.session, reasonReclaimed)
	}
	for _, k := range d.knocks {
		h.cancelKnock(k, protocol.ErrCodeMismatch, reasonReclaimed)
	}
	for w := range d.watchers {
		w.send(protocol.TypeDoorCodeChanged, "", protocol.DoorRef{DoorID: d.id})
		delete(w.watching, d.id)
	}
	d.watchers = map[*client]struct{}{}
	if old := d.host; old != nil {
		old.door = nil
		old.send(protocol.TypeDoorClose, "", protocol.SessionEnd{Reason: reasonReclaimed})
	}
	d.host = nil
	h.doorTotal.Reset(d.id)
	h.doorFails.Reset(d.id + "|anon")
	delete(h.doors, d.id)
}

// record is the door as the store keeps it. Caller holds h.mu.
func (d *door) record() store.Door {
	return store.Door{ID: d.id, KeyHash: d.keyHash, Owner: d.owner, Device: d.device, Code: d.code, Prev: d.prev, ClaimedAt: d.claimedAt, LastSeen: time.Now()}
}

func doorFrom(r store.Door) *door {
	return &door{id: r.ID, keyHash: r.KeyHash, owner: r.Owner, device: r.Device, code: r.Code, prev: r.Prev, claimedAt: r.ClaimedAt,
		closedAt: r.LastSeen, watchers: map[*client]struct{}{}, knocks: map[string]*knock{}}
}

// markDirty queues d to be written to the store. Caller holds h.mu.
func (h *Hub) markDirty(d *door) { h.dirty[d.id] = struct{}{} }

// flush writes queued doors to the store, outside h.mu. Each write is the
// door as it is now, and is conditional on its key still holding the id, so
// writes can land in any order. Losing the id to another key (another relay
// took it) closes the door here.
func (h *Hub) flush() {
	h.mu.Lock()
	if len(h.dirty) == 0 {
		h.mu.Unlock()
		return
	}
	recs := make([]store.Door, 0, len(h.dirty))
	for id := range h.dirty {
		if d, ok := h.doors[id]; ok {
			recs = append(recs, d.record())
		}
	}
	h.dirty = map[string]struct{}{}
	h.mu.Unlock()
	for _, r := range recs {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		err := h.store.Save(ctx, r, r.KeyHash)
		cancel()
		switch {
		case err == nil, errors.Is(err, store.ErrStale):
		case errors.Is(err, store.ErrTaken):
			h.lost(r.ID, r.KeyHash)
		default:
			// The heartbeat writes it again.
			slog.Warn("store save failed", "door", r.ID, "err", err)
		}
	}
}

// lost forgets a door whose id the store says another key holds now.
func (h *Hub) lost(id string, keyHash []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d, ok := h.doors[id]; ok && hmac.Equal(d.keyHash, keyHash) {
		slog.Info("door lost to another key", "door", id)
		h.evict(d)
	}
}

func (h *Hub) heartbeatLoop() {
	for range time.Tick(h.cfg.Heartbeat) {
		h.heartbeat()
	}
}

// heartbeat restarts the store TTL of every open door, so only doors whose
// host has been away for the whole TTL lapse. A record that lapsed anyway
// (store outage, eviction) is written again.
func (h *Hub) heartbeat() {
	type ref struct {
		id  string
		key []byte
	}
	h.mu.Lock()
	var open []ref
	for _, d := range h.doors {
		if d.host != nil {
			open = append(open, ref{d.id, d.keyHash})
		}
	}
	h.mu.Unlock()
	for _, r := range open {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		err := h.store.Touch(ctx, r.id, r.key)
		cancel()
		switch {
		case err == nil:
		case errors.Is(err, store.ErrNotFound):
			h.mu.Lock()
			if d, ok := h.doors[r.id]; ok {
				h.markDirty(d)
			}
			h.mu.Unlock()
		case errors.Is(err, store.ErrTaken):
			h.lost(r.id, r.key)
		default:
			slog.Warn("store heartbeat failed", "door", r.id, "err", err)
		}
	}
	h.flush()
}

// preload brings doors the request names into memory from the store when
// memory doesn't have them (after a restart), so salts, Recent and knocks
// answer as they did before it. Requests their limits would refuse anyway
// are skipped. It returns the ids the store couldn't answer for.
func (h *Hub) preload(c *client, env protocol.Envelope) map[string]bool {
	var ids []string
	kk := c.knockerKey()
	switch env.Type {
	case protocol.TypeDoorSaltGet:
		in, ok := decode[protocol.DoorSaltGet](env)
		if !ok || !doorIDRe.MatchString(in.DoorID) || h.saltLookups.Blocked(kk) {
			return nil
		}
		ids = []string{in.DoorID}
	case protocol.TypeKnockRequest:
		in, ok := decode[protocol.KnockRequest](env)
		if !ok || !doorIDRe.MatchString(in.DoorID) || h.knockSends.Blocked(kk+"|"+in.DoorID) || h.blocked(c, in.DoorID) {
			return nil
		}
		ids = []string{in.DoorID}
	case protocol.TypeRecentCheck:
		in, ok := decode[protocol.RecentCheck](env)
		if !ok || len(in.Items) > 200 || h.recentLimit.Blocked(kk) || h.knockerFails.Blocked(kk) {
			return nil
		}
		for _, it := range in.Items {
			if doorIDRe.MatchString(it.DoorID) {
				ids = append(ids, it.DoorID)
			}
		}
	default:
		return nil
	}
	h.mu.Lock()
	missing := ids[:0]
	for _, id := range ids {
		if _, ok := h.doors[id]; !ok {
			missing = append(missing, id)
		}
	}
	h.mu.Unlock()
	if len(missing) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	got, err := h.store.Load(ctx, missing)
	cancel()
	if err != nil {
		slog.Warn("store load failed", "doors", len(missing), "err", err)
		failed := make(map[string]bool, len(missing))
		for _, id := range missing {
			failed[id] = true
		}
		return failed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, r := range got {
		if _, ok := h.doors[id]; !ok && len(h.doors) < maxDoors {
			h.doors[id] = doorFrom(r)
		}
	}
	return nil
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
	d.closedAt = time.Now()
	h.markDirty(d) // the store's TTL counts from now
	h.notifyPresence(d)
}

func (h *Hub) doorCodeSet(c *client, env protocol.Envelope) {
	in, ok := decode[protocol.DoorCodeSet](env)
	if !ok || !validCode(in.Code) || c.door == nil {
		c.fail(env.ID, protocol.ErrBadRequest, "no open door or bad code")
		return
	}
	d := c.door
	if staleCode(d.code, in.Code) {
		c.failStale(env.ID, d.code.CodeVersion)
		return
	}
	if in.Code.CodeVersion != d.code.CodeVersion {
		h.setCode(d, in.Code)
	}
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

func (h *Hub) saltGet(c *client, env protocol.Envelope, failed map[string]bool) {
	in, ok := decode[protocol.DoorSaltGet](env)
	if !ok || !doorIDRe.MatchString(in.DoorID) {
		c.fail(env.ID, protocol.ErrBadRequest, "bad door id")
		return
	}
	if failed[in.DoorID] {
		// A fake salt here would turn the right code into a wrong guess.
		c.fail(env.ID, protocol.ErrRateLimited, storeDownText)
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

func (h *Hub) recentCheck(c *client, env protocol.Envelope, failed map[string]bool) {
	in, ok := decode[protocol.RecentCheck](env)
	if !ok || len(in.Items) > 200 {
		c.fail(env.ID, protocol.ErrBadRequest, "bad recent.check")
		return
	}
	seen := make(map[string]bool, len(in.Items))
	for _, it := range in.Items {
		if seen[it.DoorID] {
			c.fail(env.ID, protocol.ErrBadRequest, "bad recent.check")
			return
		}
		seen[it.DoorID] = true
	}
	kk := c.knockerKey()
	if h.recentLimit.Add(kk) > recentChecks {
		c.fail(env.ID, protocol.ErrRateLimited, "Too many checks. Wait a few minutes.")
		return
	}
	if h.knockerFails.Blocked(kk) {
		c.fail(env.ID, protocol.ErrRateLimited, "Too many wrong codes. Wait ten minutes and try again.")
		return
	}
	out := protocol.RecentStatus{Items: make([]protocol.RecentEntry, 0, len(in.Items))}
	for _, it := range in.Items {
		d, ok := h.doors[it.DoorID]
		// A hash is checked only while its knocker and door have budget
		// left, and every wrong one spends it, as on knock.request. A wrong
		// hash and an unknown door both read as offline, so this can't be
		// used to find doors. A hash from one of the door's earlier codes is
		// not a guess: that person had the code, so they're told it changed.
		match := false
		status := protocol.PresenceOffline
		// A door the store couldn't answer for reads offline and costs
		// nothing.
		if doorIDRe.MatchString(it.DoorID) && !failed[it.DoorID] && !h.blocked(c, it.DoorID) {
			switch {
			case ok && protocol.EqualHash(d.code.CodeHash, it.CodeHash):
				match = true
			case ok && d.wasCode(it.CodeHash):
				status = protocol.PresenceCodeChanged
			default:
				h.wrongCode(c, it.DoorID, it.CodeHash, d)
			}
		}
		if !match {
			if ok {
				delete(d.watchers, c)
			}
			delete(c.watching, it.DoorID)
			out.Items = append(out.Items, protocol.RecentEntry{DoorID: it.DoorID, Status: status})
			continue
		}
		d.watchers[c] = struct{}{}
		c.watching[d.id] = d
		out.Items = append(out.Items, d.entry())
	}
	c.send(protocol.TypeRecentStatus, env.ID, out)
}

func (h *Hub) knockRequest(c *client, env protocol.Envelope, failed map[string]bool) {
	in, ok := decode[protocol.KnockRequest](env)
	if !ok || !doorIDRe.MatchString(in.DoorID) {
		c.fail(env.ID, protocol.ErrBadRequest, "bad knock")
		return
	}
	if failed[in.DoorID] {
		c.fail(env.ID, protocol.ErrRateLimited, storeDownText)
		return
	}
	if c.session != nil {
		c.fail(env.ID, protocol.ErrBadRequest, "You are already in a session.")
		return
	}
	kk := c.knockerKey()
	// Counted before the hash is checked, right or wrong, so knocking over
	// and over can't flood the host with notices.
	if h.knockSends.Add(kk+"|"+in.DoorID) > knocksPerDoor {
		c.fail(env.ID, protocol.ErrRateLimited, "You've knocked a lot. Wait a few minutes and try again.")
		return
	}
	if c.knock != nil {
		h.cancelKnock(c.knock, "", "replaced")
	}
	if h.blocked(c, in.DoorID) {
		c.fail(env.ID, protocol.ErrRateLimited, "Too many wrong codes. Wait ten minutes and try again.")
		return
	}
	d, ok := h.doors[in.DoorID]
	if !ok || !protocol.EqualHash(d.code.CodeHash, in.CodeHash) {
		if !ok || !d.wasCode(in.CodeHash) {
			h.wrongCode(c, in.DoorID, in.CodeHash, d)
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
	case d.host.congested():
		c.fail(env.ID, protocol.ErrRateLimited, "They can't take knocks right now. Try again in a minute.")
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

// blocked reports whether c has used up its wrong-code budget, or the door
// its own. It is checked before any hash is compared.
func (h *Hub) blocked(c *client, doorID string) bool {
	return h.knockerFails.Blocked(c.knockerKey()) || h.doorFails.Blocked(c.failKey(doorID)) || h.doorTotal.Blocked(doorID)
}

// wrongCode spends a wrong guess against the knocker's and the door's
// budgets, whether it came from knock.request or recent.check, and whether
// or not the door exists. A knocker repeating the same wrong hash on the
// same door spends once: it tells them nothing new. d is nil for an unknown
// door.
func (h *Hub) wrongCode(c *client, doorID string, hash []byte, d *door) {
	kk := c.knockerKey()
	m := sha256.New()
	m.Write([]byte(kk + "|" + doorID + "|"))
	m.Write(hash)
	if h.seenFails.Add(hex.EncodeToString(m.Sum(nil))) > 1 {
		return
	}
	h.knockerFails.Add(kk)
	h.doorFails.Add(c.failKey(doorID))
	if h.doorTotal.Add(doorID) == doorForceNewAt && d != nil && d.host != nil {
		d.host.send(protocol.TypeDoorForceNew, "", protocol.DoorForceNewCode{Reason: "Someone tried 20 wrong codes on your door."})
	}
}

// cancelKnock removes a pending knock. errCode, if set, is sent to the guest.
func (h *Hub) cancelKnock(k *knock, errCode, reason string) {
	h.cancelKnockText(k, errCode, reasonText(errCode), reason)
}

func (h *Hub) cancelKnockText(k *knock, errCode, text, reason string) {
	k.timer.Stop()
	delete(h.knocks, k.id)
	delete(k.door.knocks, k.id)
	if k.guest.knock == k {
		k.guest.knock = nil
	}
	if errCode != "" {
		k.guest.fail(k.reqID, errCode, text)
	}
	if k.door.host != nil {
		k.door.host.sendSoft(protocol.TypeKnockCancelled, "", protocol.KnockRef{KnockID: k.id, Reason: reason})
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
	if g.session != nil {
		// They started or joined another session after knocking. One
		// socket carries one session, or the first would lose its controls.
		h.cancelKnockText(k, protocol.ErrDoorBusy, "You're already in a session.", "busy")
		c.fail(env.ID, protocol.ErrDoorBusy, "They're in another session now.")
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
	if c.knock != nil {
		// The host was waiting at someone else's door; it can't be both.
		h.cancelKnockText(c.knock, protocol.ErrDoorBusy, "You let someone in, so your knock was cancelled.", "cancelled")
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

// endReason picks the reason both sides see. A host may say why it ended
// (its shell exited, it closed the door, it quit); anything else is host_ended.
// A guest always just left.
func endReason(isHost bool, asked string) string {
	if !isHost {
		return protocol.EndGuestLeft
	}
	switch asked {
	case protocol.EndShellExited, protocol.EndDoorClosed, protocol.EndHostQuit:
		return asked
	}
	return protocol.EndHostEnded
}

func (h *Hub) disconnect(c *client) {
	h.mu.Lock()
	defer h.flush()
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
	// Hold the sender back while the peer has maxQueuedBytes waiting, so a
	// slow reader can't pin a full queue of 1 MiB frames in memory. Only
	// this sender's read loop forwards to this peer, so nothing else adds
	// session bytes to its queue meanwhile.
	n := int64(len(b))
	stuck := time.NewTimer(15 * time.Second)
	defer stuck.Stop()
	for q := to.queued.Load(); q > 0 && q+n > maxQueuedBytes; q = to.queued.Load() {
		select {
		case <-to.drained:
		case <-to.ctx.Done():
			return
		case <-c.ctx.Done():
			return
		case <-stuck.C:
			to.cancel() // peer stopped reading
			return
		}
	}
	to.queued.Add(n)
	select {
	case to.out <- frame{websocket.MessageBinary, b}:
		return
	case <-to.ctx.Done():
	case <-c.ctx.Done():
	case <-stuck.C:
		to.cancel() // peer stopped reading
	}
	to.queued.Add(-n)
}
