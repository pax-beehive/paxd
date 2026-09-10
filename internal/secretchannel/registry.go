package secretchannel

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

var (
	ErrRateLimited     = errors.New("secretchannel: too many channels opened recently")
	ErrTooManyChannels = errors.New("secretchannel: too many open channels")
)

// Identity is the caller identity a channel is bound to. It is deliberately
// an opaque string supplied by whatever authenticated the request (e.g. the
// control tunnel's remote id) — this package does not know about users,
// nodes-as-a-concept, or manager accounts, and must not be trusted to
// authenticate anything itself.
type Identity struct {
	Principal string
}

// ChannelInfo is returned to the caller of Open. PublicKey is safe to hand
// to an untrusted browser: it carries no confidentiality requirement, only
// an integrity one (already satisfied by the authenticated transport that
// relays it).
//
// NodeID is whatever this Registry was configured with (see Options); it is
// an AAD-binding value, not necessarily a manager-assigned node identity.
// Callers must echo it back verbatim in the PushContext used to seal the
// secret — they must never independently supply their own idea of "this
// node's ID", since that will not match what Consume reconstructs and the
// decrypt will fail closed.
type ChannelInfo struct {
	ChannelID string
	NodeID    string
	PublicKey []byte
	ExpiresAt time.Time
}

// PushRequest is the sealed payload a caller submits to redeem a channel.
type PushRequest struct {
	ChannelID       string
	CommandID       string
	SenderPublicKey []byte
	Nonce           []byte
	Ciphertext      []byte
}

type Status string

const (
	StatusApplied        Status = "applied"
	StatusExpired        Status = "expired"
	StatusConsumed       Status = "consumed"
	StatusUnauthorized   Status = "unauthorized"
	StatusConflict       Status = "conflict"
	StatusInvalidPayload Status = "invalid_payload"
	StatusWriteFailed    Status = "write_failed"
)

// PushResult never carries plaintext.
type PushResult struct {
	Status    Status
	FileRef   string
	ExpiresAt time.Time
}

// Writer persists a decrypted secret and returns a reference to it. It is
// called at most once per successfully consumed channel.
type Writer func(plaintext []byte) (fileRef string, expiresAt time.Time, err error)

type Options struct {
	NodeID          string
	ChannelTTL      time.Duration
	ResultRetention time.Duration
	MaxChannels     int
	RateLimit       int
	RateWindow      time.Duration
	Now             func() time.Time
	Random          io.Reader
	Writer          Writer
}

// maxCiphertextBytes bounds request size before any crypto runs, so an
// oversized body cannot be used to force expensive work or exhaust memory.
const maxCiphertextBytes = MaxPlaintextBytes + 64

type channelEntry struct {
	identity  Identity
	priv      *ecdh.PrivateKey
	pub       []byte
	expiresAt time.Time
}

type resultCacheKey struct {
	principal string
	channelID string
	commandID string
}

// pendingResult is created (under the registry lock) the instant a request
// wins the right to consume a channel, before the slow decrypt/write work
// starts. A concurrent duplicate request for the same (principal, channel,
// command) — e.g. a client retry after a lost ACK — looks this up and waits
// on ready instead of racing past the now-deleted channel entry and getting
// a spurious "expired" for work that is already in flight or already done.
type pendingResult struct {
	ready    chan struct{}
	digest   [sha256.Size]byte
	result   PushResult
	cachedAt time.Time
}

// Registry holds every open (unconsumed) channel and a short-lived
// dedup cache of push results. Nothing here is persisted: a paxd restart
// invalidates every outstanding channel, which is intentional (see the
// package doc).
type Registry struct {
	opts Options

	mu       sync.Mutex
	channels map[string]*channelEntry
	results  map[resultCacheKey]*pendingResult
	// consumed records every channel_id that has ever been claimed, keyed
	// by channel_id alone (not by principal/command_id): once a channel is
	// claimed it is gone for good regardless of who asks about it or what
	// command_id they use, and callers must not treat "channel not found"
	// as safe-to-retry when it actually means "already used". See Consume.
	consumed map[string]time.Time
	opens    map[string][]time.Time
}

func NewRegistry(opts Options) *Registry {
	if opts.ChannelTTL <= 0 {
		opts.ChannelTTL = 5 * time.Minute
	}
	if opts.ResultRetention <= 0 {
		opts.ResultRetention = 15 * time.Minute
	}
	if opts.MaxChannels <= 0 {
		opts.MaxChannels = 64
	}
	if opts.RateLimit <= 0 {
		opts.RateLimit = 5
	}
	if opts.RateWindow <= 0 {
		opts.RateWindow = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	return &Registry{
		opts:     opts,
		channels: make(map[string]*channelEntry),
		results:  make(map[resultCacheKey]*pendingResult),
		consumed: make(map[string]time.Time),
		opens:    make(map[string][]time.Time),
	}
}

// Open generates a fresh ECDH(P-256) keypair, holds the private key in
// memory only, and returns the public key plus an expiry. The private key
// is destroyed the moment Consume redeems or expires the channel.
func (r *Registry) Open(identity Identity) (ChannelInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.opts.Now()
	r.sweepLocked(now)

	if !r.allowOpenLocked(identity, now) {
		return ChannelInfo{}, ErrRateLimited
	}
	if len(r.channels) >= r.opts.MaxChannels {
		return ChannelInfo{}, ErrTooManyChannels
	}

	priv, err := ecdh.P256().GenerateKey(r.opts.Random)
	if err != nil {
		return ChannelInfo{}, err
	}
	channelID, err := randomID(r.opts.Random)
	if err != nil {
		return ChannelInfo{}, err
	}
	expiresAt := now.Add(r.opts.ChannelTTL)
	r.channels[channelID] = &channelEntry{
		identity:  identity,
		priv:      priv,
		pub:       priv.PublicKey().Bytes(),
		expiresAt: expiresAt,
	}
	return ChannelInfo{
		ChannelID: channelID,
		NodeID:    r.opts.NodeID,
		PublicKey: priv.PublicKey().Bytes(),
		ExpiresAt: expiresAt,
	}, nil
}

// Consume redeems a channel exactly once. Authorized attempts that fail to
// decrypt or fail to persist still burn the channel (no decryption oracle);
// unauthorized attempts (wrong identity) do not touch it, so the rightful
// owner can still use it.
func (r *Registry) Consume(identity Identity, req PushRequest) (PushResult, error) {
	if err := validatePushRequest(req); err != nil {
		return PushResult{Status: StatusInvalidPayload}, nil
	}

	digest := requestDigest(req)
	key := resultCacheKey{principal: identity.Principal, channelID: req.ChannelID, commandID: req.CommandID}

	r.mu.Lock()
	now := r.opts.Now()
	r.sweepLocked(now)

	if pending, ok := r.results[key]; ok {
		r.mu.Unlock()
		<-pending.ready // no-op if already closed
		if pending.digest != digest {
			return PushResult{Status: StatusConflict}, nil
		}
		return pending.result, nil
	}

	if consumedAt, ok := r.consumed[req.ChannelID]; ok && now.Sub(consumedAt) <= r.opts.ResultRetention {
		// This exact channel_id was already claimed, just not under this
		// (principal, command_id) pair — e.g. a caller retrying with a
		// freshly generated command_id instead of the one it originally
		// used. Report it distinctly from "expired": expired means safe to
		// open a new channel and try again, consumed does not, since
		// whatever the original attempt did (succeed, fail to decrypt,
		// fail to write) cannot be undone or safely repeated.
		r.mu.Unlock()
		return PushResult{Status: StatusConsumed}, nil
	}

	entry, ok := r.channels[req.ChannelID]
	if !ok || now.After(entry.expiresAt) {
		delete(r.channels, req.ChannelID)
		r.mu.Unlock()
		return PushResult{Status: StatusExpired}, nil
	}
	if entry.identity != identity {
		r.mu.Unlock()
		return PushResult{Status: StatusUnauthorized}, nil
	}

	// Claim the channel now, before doing any (potentially slow) crypto or
	// I/O: publish a pending placeholder for this (principal, channel,
	// command) under the same lock that deletes the channel entry, so a
	// concurrent duplicate request (e.g. a client retry racing the original
	// after a slow response) finds the placeholder and waits for the real
	// result instead of seeing a channel that just vanished and concluding
	// "expired" — which would otherwise send it off to open a second
	// channel and re-deliver the secret a second time.
	pending := &pendingResult{ready: make(chan struct{}), digest: digest}
	r.results[key] = pending
	r.consumed[req.ChannelID] = now
	delete(r.channels, req.ChannelID)
	priv := entry.priv
	nodeID := r.opts.NodeID
	expiresAt := entry.expiresAt
	r.mu.Unlock()

	result := r.performConsume(priv, nodeID, expiresAt, req)

	r.mu.Lock()
	pending.result = result
	pending.cachedAt = r.opts.Now()
	close(pending.ready)
	r.mu.Unlock()
	return result, nil
}

func (r *Registry) performConsume(priv *ecdh.PrivateKey, nodeID string, expiresAt time.Time, req PushRequest) PushResult {
	plaintext, err := OpenSecret(priv, SealedMessage{
		SenderPublicKey: req.SenderPublicKey,
		Nonce:           req.Nonce,
		Ciphertext:      req.Ciphertext,
	}, PushContext{
		NodeID:        nodeID,
		ChannelID:     req.ChannelID,
		CommandID:     req.CommandID,
		ExpiresAtUnix: expiresAt.Unix(),
	})
	if err != nil {
		return PushResult{Status: StatusInvalidPayload}
	}
	defer zero(plaintext)

	if r.opts.Writer == nil {
		return PushResult{Status: StatusWriteFailed}
	}
	fileRef, fileExpiresAt, err := r.opts.Writer(plaintext)
	if err != nil {
		return PushResult{Status: StatusWriteFailed}
	}
	return PushResult{Status: StatusApplied, FileRef: fileRef, ExpiresAt: fileExpiresAt}
}

// Sweep drops expired channels and stale cached results. Callers should
// invoke it periodically (e.g. once a minute); Open and Consume also sweep
// opportunistically so correctness never depends on the caller doing so.
func (r *Registry) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(r.opts.Now())
}

func (r *Registry) sweepLocked(now time.Time) {
	for id, entry := range r.channels {
		if now.After(entry.expiresAt) {
			delete(r.channels, id)
		}
	}
	for key, pending := range r.results {
		if pending.cachedAt.IsZero() {
			continue // still being computed; never sweep a result in flight
		}
		if now.Sub(pending.cachedAt) > r.opts.ResultRetention {
			delete(r.results, key)
		}
	}
	for id, consumedAt := range r.consumed {
		if now.Sub(consumedAt) > r.opts.ResultRetention {
			delete(r.consumed, id)
		}
	}
	cutoff := now.Add(-r.opts.RateWindow)
	for principal, hits := range r.opens {
		kept := hits[:0]
		for _, t := range hits {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(r.opens, principal)
		} else {
			r.opens[principal] = kept
		}
	}
}

func (r *Registry) allowOpenLocked(identity Identity, now time.Time) bool {
	cutoff := now.Add(-r.opts.RateWindow)
	hits := r.opens[identity.Principal]
	kept := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= r.opts.RateLimit {
		r.opens[identity.Principal] = kept
		return false
	}
	r.opens[identity.Principal] = append(kept, now)
	return true
}

func validatePushRequest(req PushRequest) error {
	if req.ChannelID == "" || req.CommandID == "" {
		return errors.New("secretchannel: channel_id and command_id are required")
	}
	if len(req.SenderPublicKey) == 0 || len(req.Nonce) == 0 || len(req.Ciphertext) == 0 {
		return errors.New("secretchannel: sender_public_key, nonce, and ciphertext are required")
	}
	if len(req.Ciphertext) > maxCiphertextBytes {
		return errors.New("secretchannel: ciphertext too large")
	}
	return nil
}

func requestDigest(req PushRequest) [sha256.Size]byte {
	encoded, _ := json.Marshal([]any{
		req.ChannelID,
		req.CommandID,
		req.SenderPublicKey,
		req.Nonce,
		req.Ciphertext,
	})
	return sha256.Sum256(encoded)
}

func randomID(random io.Reader) (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// zero is a best-effort scrub, not a guarantee: Go may have already copied
// this slice's contents elsewhere (e.g. into req.Ciphertext's decrypted
// buffer aliasing, GC-moved backing arrays do not apply here but escape
// analysis and compiler optimizations can still leave copies). See the plan
// doc's threat model: no reliable erasure is promised.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
