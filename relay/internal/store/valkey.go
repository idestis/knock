package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Valkey keeps records in Valkey (or anything that speaks its protocol), so
// door ownership survives a relay restart. Each door is a hash at
// <prefix><id> with fields key (hex key hash), ver (code version), seen
// (unix ms) and data (the JSON record), expiring TTL after its last write.
// Writes are Lua scripts, so the holder check and the write are one step.
type Valkey struct {
	c      valkey.Client
	ttl    time.Duration
	prefix string
}

// DefaultPrefix namespaces knockd's keys in a shared Valkey.
const DefaultPrefix = "knock:door:"

// ErrBadURL is returned for a URL that doesn't parse. It never carries the
// URL, which may hold a password.
var ErrBadURL = errors.New("store: KNOCK_VALKEY_URL is not a valid redis://, rediss:// or unix:// URL")

// NewValkey connects to url (redis://, rediss:// or unix://). Errors never
// include the URL.
func NewValkey(url string, ttl time.Duration, prefix string) (*Valkey, error) {
	opt, err := valkey.ParseURL(url)
	if err != nil {
		return nil, ErrBadURL
	}
	// knockd never reads through the client-side cache, and some managed
	// services don't support the tracking it needs.
	opt.DisableCache = true
	c, err := valkey.NewClient(opt)
	if err != nil {
		return nil, errors.New("store: can't reach Valkey: " + redact(err, url, opt.Password))
	}
	if prefix == "" {
		prefix = DefaultPrefix
	}
	return &Valkey{c: c, ttl: ttl, prefix: prefix}, nil
}

// redact keeps a connection error from ever printing the URL or password.
func redact(err error, secrets ...string) string {
	s := err.Error()
	for _, x := range secrets {
		if x != "" && strings.Contains(s, x) {
			return "connection failed"
		}
	}
	return s
}

func (v *Valkey) key(id string) string { return v.prefix + id }

// saveScript: ARGV = holder hex, key hex, code version, seen ms, data, ttl ms.
// Returns 1 written, 0 held by another key, -1 same key with a newer code.
var saveScript = valkey.NewLuaScript(`
local cur = redis.call('HMGET', KEYS[1], 'key', 'ver')
if cur[1] then
  if cur[1] ~= ARGV[1] then return 0 end
  if cur[1] == ARGV[2] and tonumber(cur[2]) > tonumber(ARGV[3]) then return -1 end
end
redis.call('DEL', KEYS[1])
redis.call('HSET', KEYS[1], 'key', ARGV[2], 'ver', ARGV[3], 'seen', ARGV[4], 'data', ARGV[5])
redis.call('PEXPIRE', KEYS[1], ARGV[6])
return 1
`)

// touchScript: ARGV = key hex, seen ms, ttl ms. Returns 1 touched, 0 gone,
// -1 held by another key.
var touchScript = valkey.NewLuaScript(`
local k = redis.call('HGET', KEYS[1], 'key')
if not k then return 0 end
if k ~= ARGV[1] then return -1 end
redis.call('HSET', KEYS[1], 'seen', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

func (v *Valkey) Load(ctx context.Context, ids []string) (map[string]Door, error) {
	out := map[string]Door{}
	if len(ids) == 0 {
		return out, nil
	}
	cmds := make(valkey.Commands, 0, len(ids))
	for _, id := range ids {
		cmds = append(cmds, v.c.B().Hmget().Key(v.key(id)).Field("data", "seen").Build())
	}
	for i, r := range v.c.DoMulti(ctx, cmds...) {
		vals, err := r.ToArray()
		if err != nil {
			return nil, err
		}
		if len(vals) != 2 {
			continue
		}
		data, err := vals[0].ToString()
		if valkey.IsValkeyNil(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var d Door
		if json.Unmarshal([]byte(data), &d) != nil || d.ID != ids[i] {
			continue // unreadable: treat as absent rather than trust it
		}
		if s, err := vals[1].ToString(); err == nil {
			if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
				d.LastSeen = time.UnixMilli(ms)
			}
		}
		out[ids[i]] = d
	}
	return out, nil
}

func (v *Valkey) Save(ctx context.Context, d Door, holder []byte) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	seen := d.LastSeen
	if seen.IsZero() {
		seen = time.Now()
	}
	n, err := saveScript.Exec(ctx, v.c, []string{v.key(d.ID)}, []string{
		hex.EncodeToString(holder), hex.EncodeToString(d.KeyHash), strconv.FormatInt(d.Code.CodeVersion, 10),
		strconv.FormatInt(seen.UnixMilli(), 10), string(data), strconv.FormatInt(v.ttl.Milliseconds(), 10),
	}).AsInt64()
	switch {
	case err != nil:
		return err
	case n == 0:
		return ErrTaken
	case n < 0:
		return ErrStale
	}
	return nil
}

func (v *Valkey) Touch(ctx context.Context, id string, keyHash []byte) error {
	n, err := touchScript.Exec(ctx, v.c, []string{v.key(id)}, []string{
		hex.EncodeToString(keyHash), strconv.FormatInt(time.Now().UnixMilli(), 10), strconv.FormatInt(v.ttl.Milliseconds(), 10),
	}).AsInt64()
	switch {
	case err != nil:
		return err
	case n == 0:
		return ErrNotFound
	case n < 0:
		return ErrTaken
	}
	return nil
}

func (v *Valkey) Close() { v.c.Close() }
