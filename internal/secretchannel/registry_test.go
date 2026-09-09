package secretchannel

import (
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errDiskFull = errors.New("disk full")

// fakeClock lets tests deterministically advance time without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type recordingWriter struct {
	mu    sync.Mutex
	calls int
	last  []byte
	fn    func(plaintext []byte) (string, time.Time, error)
}

func (w *recordingWriter) Write(plaintext []byte) (string, time.Time, error) {
	w.mu.Lock()
	w.calls++
	w.last = append([]byte(nil), plaintext...)
	fn := w.fn
	w.mu.Unlock()
	if fn != nil {
		return fn(plaintext)
	}
	return "file:/tmp/secretchannel-test", time.Now().Add(time.Minute), nil
}

func (w *recordingWriter) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func newTestRegistry(t *testing.T, writer *recordingWriter, clock *fakeClock) *Registry {
	t.Helper()
	if clock == nil {
		clock = newFakeClock()
	}
	return NewRegistry(Options{
		NodeID:          "node_1",
		ChannelTTL:      5 * time.Minute,
		ResultRetention: 15 * time.Minute,
		MaxChannels:     64,
		RateLimit:       5,
		RateWindow:      time.Minute,
		Now:             clock.Now,
		Random:          rand.Reader,
		Writer:          writer.Write,
	})
}

func sealForChannel(t *testing.T, info ChannelInfo, nodeID, commandID string, plaintext []byte) PushRequest {
	t.Helper()
	sealed, err := SealSecret(info.PublicKey, plaintext, PushContext{
		NodeID:        nodeID,
		ChannelID:     info.ChannelID,
		CommandID:     commandID,
		ExpiresAtUnix: info.ExpiresAt.Unix(),
	}, rand.Reader)
	require.NoError(t, err)
	return PushRequest{
		ChannelID:       info.ChannelID,
		CommandID:       commandID,
		SenderPublicKey: sealed.SenderPublicKey,
		Nonce:           sealed.Nonce,
		Ciphertext:      sealed.Ciphertext,
	}
}

func TestOpenReturnsValidChannel(t *testing.T) {
	writer := &recordingWriter{}
	clock := newFakeClock()
	reg := newTestRegistry(t, writer, clock)

	info, err := reg.Open(Identity{Principal: "remote_a"})
	require.NoError(t, err)
	require.NotEmpty(t, info.ChannelID)
	require.NotEmpty(t, info.PublicKey)
	require.Equal(t, clock.Now().Add(5*time.Minute), info.ExpiresAt)
}

func TestConsumeAppliesAndWritesOnce(t *testing.T) {
	writer := &recordingWriter{}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))

	result, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, StatusApplied, result.Status)
	require.NotEmpty(t, result.FileRef)
	require.Equal(t, 1, writer.callCount())
	require.Equal(t, []byte("sk-secret"), writer.last)
}

func TestConsumeIsSingleUse(t *testing.T) {
	writer := &recordingWriter{}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	first := sealForChannel(t, info, "node_1", "cmd_1", []byte("first"))
	result, err := reg.Consume(identity, first)
	require.NoError(t, err)
	require.Equal(t, StatusApplied, result.Status)

	// A different push (different command_id) against the same, now-burned
	// channel must not succeed, even though it is validly encrypted.
	second := sealForChannel(t, info, "node_1", "cmd_2", []byte("second"))
	result2, err := reg.Consume(identity, second)
	require.NoError(t, err)
	require.NotEqual(t, StatusApplied, result2.Status)
	require.Equal(t, 1, writer.callCount(), "the channel must not be usable twice")
}

func TestConsumeRetryWithSameRequestReturnsCachedResult(t *testing.T) {
	writer := &recordingWriter{}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))

	first, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, StatusApplied, first.Status)

	// e.g. the ACK was lost and the caller retries the identical command.
	second, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, first, second, "an identical retry must be idempotent")
	require.Equal(t, 1, writer.callCount(), "idempotent retry must not write again")
}

func TestConsumeSameCommandIDDifferentPayloadIsConflict(t *testing.T) {
	writer := &recordingWriter{}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))

	first, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, StatusApplied, first.Status)

	mutated := req
	mutated.Ciphertext = append([]byte(nil), req.Ciphertext...)
	mutated.Ciphertext[0] ^= 0xFF

	second, err := reg.Consume(identity, mutated)
	require.NoError(t, err)
	require.Equal(t, StatusConflict, second.Status)
	require.Equal(t, 1, writer.callCount())
}

func TestConsumeExpiredChannel(t *testing.T) {
	writer := &recordingWriter{}
	clock := newFakeClock()
	reg := newTestRegistry(t, writer, clock)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))

	clock.Advance(6 * time.Minute)

	result, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, StatusExpired, result.Status)
	require.Equal(t, 0, writer.callCount())
}

func TestConsumeUnknownChannelID(t *testing.T) {
	writer := &recordingWriter{}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	// Never opened; still must be a well-formed request to reach the lookup.
	info, err := reg.Open(identity)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))
	req.ChannelID = "chan_does_not_exist"

	result, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, StatusExpired, result.Status)
	require.Equal(t, 0, writer.callCount())
}

func TestConsumeUnauthorizedPrincipalDoesNotBurnChannel(t *testing.T) {
	writer := &recordingWriter{}
	reg := newTestRegistry(t, writer, nil)
	owner := Identity{Principal: "remote_a"}
	attacker := Identity{Principal: "remote_b"}

	info, err := reg.Open(owner)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))

	unauthorizedResult, err := reg.Consume(attacker, req)
	require.NoError(t, err)
	require.Equal(t, StatusUnauthorized, unauthorizedResult.Status)
	require.Equal(t, 0, writer.callCount())

	// The rightful owner must still be able to use the channel.
	ownedResult, err := reg.Consume(owner, req)
	require.NoError(t, err)
	require.Equal(t, StatusApplied, ownedResult.Status)
	require.Equal(t, 1, writer.callCount())
}

func TestConsumeInvalidCiphertextBurnsChannel(t *testing.T) {
	writer := &recordingWriter{}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))
	req.Ciphertext = append([]byte(nil), req.Ciphertext...)
	req.Ciphertext[0] ^= 0xFF

	result, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, StatusInvalidPayload, result.Status)
	require.Equal(t, 0, writer.callCount())

	retry := sealForChannel(t, info, "node_1", "cmd_2", []byte("sk-secret"))
	retryResult, err := reg.Consume(identity, retry)
	require.NoError(t, err)
	require.NotEqual(t, StatusApplied, retryResult.Status, "a single-attempt oracle must not allow retrying after a bad decrypt")
}

func TestConsumeWriteFailureIsCachedAndNotRetried(t *testing.T) {
	writer := &recordingWriter{fn: func(plaintext []byte) (string, time.Time, error) {
		return "", time.Time{}, errDiskFull
	}}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))

	result, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, StatusWriteFailed, result.Status)

	retry, err := reg.Consume(identity, req)
	require.NoError(t, err)
	require.Equal(t, result, retry)
	require.Equal(t, 1, writer.callCount())
}

func TestConsumeIsExclusiveUnderConcurrency(t *testing.T) {
	release := make(chan struct{})
	var writeCount int32
	writer := &recordingWriter{fn: func(plaintext []byte) (string, time.Time, error) {
		atomic.AddInt32(&writeCount, 1)
		<-release
		return "file:/tmp/secretchannel-test", time.Now().Add(time.Minute), nil
	}}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	req := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))

	var wg sync.WaitGroup
	results := make([]PushResult, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := reg.Consume(identity, req)
			require.NoError(t, err)
			results[i] = result
		}(i)
	}
	// Give both goroutines a chance to reach the lookup before either
	// finishes the (blocked) write, so they genuinely race.
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()

	require.EqualValues(t, 1, atomic.LoadInt32(&writeCount), "the private key must only ever be used once")
	// Both callers submitted the identical request, so both must observe
	// the same successful outcome: the loser waits for the winner's result
	// instead of racing past the deleted channel entry and seeing
	// "expired" for work that actually succeeded (or is still in flight).
	require.Equal(t, results[0], results[1])
	require.Equal(t, StatusApplied, results[0].Status)
}

func TestConsumeConcurrentDuplicateWithDifferentPayloadIsConflictNotExpired(t *testing.T) {
	release := make(chan struct{})
	writer := &recordingWriter{fn: func(plaintext []byte) (string, time.Time, error) {
		<-release
		return "file:/tmp/secretchannel-test", time.Now().Add(time.Minute), nil
	}}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	first := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))
	mutated := first
	mutated.Ciphertext = append([]byte(nil), first.Ciphertext...)
	mutated.Ciphertext[0] ^= 0xFF

	var firstResult, secondResult PushResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		result, consumeErr := reg.Consume(identity, first)
		require.NoError(t, consumeErr)
		firstResult = result
	}()
	time.Sleep(5 * time.Millisecond) // ensure `first` claims the channel before `mutated` arrives
	go func() {
		defer wg.Done()
		result, consumeErr := reg.Consume(identity, mutated)
		require.NoError(t, consumeErr)
		secondResult = result
	}()
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()

	require.Equal(t, StatusApplied, firstResult.Status)
	require.Equal(t, StatusConflict, secondResult.Status, "a concurrent request with different content for the same command_id must never be told 'expired'")
}

func TestOpenEchoesConfiguredNodeID(t *testing.T) {
	writer := &recordingWriter{}
	reg := NewRegistry(Options{
		NodeID:      "node_from_registration",
		ChannelTTL:  5 * time.Minute,
		MaxChannels: 64,
		RateLimit:   5,
		RateWindow:  time.Minute,
		Random:      rand.Reader,
		Writer:      writer.Write,
	})

	info, err := reg.Open(Identity{Principal: "remote_a"})
	require.NoError(t, err)
	require.Equal(t, "node_from_registration", info.NodeID)
}

func TestConsumeFailsIfCallerSuppliesItsOwnNodeIDInsteadOfEchoingOpen(t *testing.T) {
	writer := &recordingWriter{}
	reg := NewRegistry(Options{
		NodeID:      "node_from_registration",
		ChannelTTL:  5 * time.Minute,
		MaxChannels: 64,
		RateLimit:   5,
		RateWindow:  time.Minute,
		Random:      rand.Reader,
		Writer:      writer.Write,
	})
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	require.NotEqual(t, "node_id_the_frontend_guessed", info.NodeID)

	// A caller that seals against a node_id it invented itself (e.g. a
	// domain node ID it already knew, rather than echoing info.NodeID)
	// produces AAD paxd cannot reconstruct: decryption must fail closed,
	// not silently succeed against the wrong context.
	wrongContextReq := sealForChannel(t, info, "node_id_the_frontend_guessed", "cmd_1", []byte("sk-secret"))
	result, err := reg.Consume(identity, wrongContextReq)
	require.NoError(t, err)
	require.Equal(t, StatusInvalidPayload, result.Status)
}

func TestOpenIsRateLimited(t *testing.T) {
	writer := &recordingWriter{}
	clock := newFakeClock()
	reg := newTestRegistry(t, writer, clock)
	identity := Identity{Principal: "remote_a"}

	for i := 0; i < 5; i++ {
		_, err := reg.Open(identity)
		require.NoError(t, err)
	}
	_, err := reg.Open(identity)
	require.ErrorIs(t, err, ErrRateLimited)

	// A different principal has its own budget.
	_, err = reg.Open(Identity{Principal: "remote_b"})
	require.NoError(t, err)

	clock.Advance(time.Minute + time.Second)
	_, err = reg.Open(identity)
	require.NoError(t, err, "the rate limit window must roll forward")
}

func TestOpenEnforcesMaxChannels(t *testing.T) {
	writer := &recordingWriter{}
	reg := NewRegistry(Options{
		NodeID:      "node_1",
		ChannelTTL:  5 * time.Minute,
		MaxChannels: 1,
		RateLimit:   10,
		RateWindow:  time.Minute,
		Random:      rand.Reader,
		Writer:      writer.Write,
	})

	_, err := reg.Open(Identity{Principal: "remote_a"})
	require.NoError(t, err)
	_, err = reg.Open(Identity{Principal: "remote_b"})
	require.ErrorIs(t, err, ErrTooManyChannels)
}

func TestSweepRemovesExpiredChannels(t *testing.T) {
	writer := &recordingWriter{}
	clock := newFakeClock()
	reg := newTestRegistry(t, writer, clock)

	_, err := reg.Open(Identity{Principal: "remote_a"})
	require.NoError(t, err)
	require.Len(t, reg.channels, 1)

	clock.Advance(6 * time.Minute)
	reg.Sweep()
	require.Len(t, reg.channels, 0)
}

func TestConsumeRejectsMalformedRequest(t *testing.T) {
	writer := &recordingWriter{}
	reg := newTestRegistry(t, writer, nil)
	identity := Identity{Principal: "remote_a"}

	info, err := reg.Open(identity)
	require.NoError(t, err)
	base := sealForChannel(t, info, "node_1", "cmd_1", []byte("sk-secret"))

	cases := []struct {
		name string
		req  PushRequest
	}{
		{"missing command id", PushRequest{ChannelID: base.ChannelID, SenderPublicKey: base.SenderPublicKey, Nonce: base.Nonce, Ciphertext: base.Ciphertext}},
		{"missing sender key", PushRequest{ChannelID: base.ChannelID, CommandID: "cmd_1", Nonce: base.Nonce, Ciphertext: base.Ciphertext}},
		{"missing nonce", PushRequest{ChannelID: base.ChannelID, CommandID: "cmd_1", SenderPublicKey: base.SenderPublicKey, Ciphertext: base.Ciphertext}},
		{"missing ciphertext", PushRequest{ChannelID: base.ChannelID, CommandID: "cmd_1", SenderPublicKey: base.SenderPublicKey, Nonce: base.Nonce}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := reg.Consume(identity, tc.req)
			require.NoError(t, err)
			require.Equal(t, StatusInvalidPayload, result.Status)
		})
	}
	require.Equal(t, 0, writer.callCount())

	// The channel must still be intact: malformed requests fail validation
	// before ever touching it.
	final, err := reg.Consume(identity, base)
	require.NoError(t, err)
	require.Equal(t, StatusApplied, final.Status)
}
