package shhext

import (
	"context"
	"hash/fnv"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/status-im/status-go/services/shhext/mailservers"
	whisper "github.com/status-im/whisper/whisperv6"
)

const (
	maxAttempts = 3
)

func messageID(message whisper.NewMessage) common.Hash {
	hash := fnv.New32()
	hash.Write(message.Payload)
	hash.Write(message.Topic[:])
	return common.BytesToHash(hash.Sum(nil))
}

// NewEnvelopesMonitor returns a pointer to an instance of the EnvelopesMonitor.
func NewEnvelopesMonitor(w *whisper.Whisper, handler EnvelopeEventsHandler, mailServerConfirmation bool, mailPeers *mailservers.PeerStore) *EnvelopesMonitor {
	return &EnvelopesMonitor{
		w:                      w,
		whisperAPI:             whisper.NewPublicWhisperAPI(w),
		handler:                handler,
		mailServerConfirmation: mailServerConfirmation,
		mailPeers:              mailPeers,

		envelopes:         map[common.Hash]EnvelopeState{},
		batches:           map[common.Hash]map[common.Hash]struct{}{},
		messages:          map[common.Hash]whisper.NewMessage{},
		attempts:          map[common.Hash]int{},
		messageToEnvelope: map[common.Hash]common.Hash{},
	}
}

// EnvelopesMonitor is responsible for monitoring whisper envelopes state.
type EnvelopesMonitor struct {
	w                      *whisper.Whisper
	whisperAPI             *whisper.PublicWhisperAPI
	handler                EnvelopeEventsHandler
	mailServerConfirmation bool

	mu        sync.Mutex
	envelopes map[common.Hash]EnvelopeState
	batches   map[common.Hash]map[common.Hash]struct{}

	messageToEnvelope map[common.Hash]common.Hash
	messages          map[common.Hash]whisper.NewMessage
	attempts          map[common.Hash]int

	mailPeers *mailservers.PeerStore

	wg   sync.WaitGroup
	quit chan struct{}
}

// Start processing events.
func (m *EnvelopesMonitor) Start() {
	m.quit = make(chan struct{})
	m.wg.Add(1)
	go func() {
		m.handleEnvelopeEvents()
		m.wg.Done()
	}()
}

// Stop process events.
func (m *EnvelopesMonitor) Stop() {
	close(m.quit)
	m.wg.Wait()
}

// Add hash to a tracker.
func (m *EnvelopesMonitor) Add(envelopeHash common.Hash, message whisper.NewMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.envelopes[envelopeHash] = EnvelopePosted
	m.messages[envelopeHash] = message
	m.attempts[envelopeHash] = 1
	m.messageToEnvelope[messageID(message)] = envelopeHash
}

func (m *EnvelopesMonitor) GetState(hash common.Hash) EnvelopeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, exist := m.envelopes[hash]
	if !exist {
		return NotRegistered
	}
	return state
}

func (m *EnvelopesMonitor) GetMessageState(mID common.Hash) EnvelopeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	envelope, exist := m.messageToEnvelope[mID]
	if !exist {
		return NotRegistered
	}
	state, exist := m.envelopes[envelope]
	if !exist {
		return NotRegistered
	}
	return state
}

// handleEnvelopeEvents processes whisper envelope events
func (m *EnvelopesMonitor) handleEnvelopeEvents() {
	events := make(chan whisper.EnvelopeEvent, 100) // must be buffered to prevent blocking whisper
	sub := m.w.SubscribeEnvelopeEvents(events)
	defer sub.Unsubscribe()
	for {
		select {
		case <-m.quit:
			return
		case event := <-events:
			m.handleEvent(event)
		}
	}
}

// handleEvent based on type of the event either triggers
// confirmation handler or removes hash from tracker
func (m *EnvelopesMonitor) handleEvent(event whisper.EnvelopeEvent) {
	handlers := map[whisper.EventType]func(whisper.EnvelopeEvent){
		whisper.EventEnvelopeSent:      m.handleEventEnvelopeSent,
		whisper.EventEnvelopeExpired:   m.handleEventEnvelopeExpired,
		whisper.EventBatchAcknowledged: m.handleAcknowledgedBatch,
		whisper.EventEnvelopeReceived:  m.handleEventEnvelopeReceived,
	}
	if handler, ok := handlers[event.Event]; ok {
		handler(event)
	}
}

func (m *EnvelopesMonitor) handleEventEnvelopeSent(event whisper.EnvelopeEvent) {
	if m.mailServerConfirmation {
		if !m.isMailserver(event.Peer) {
			return
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.envelopes[event.Hash]
	// if we didn't send a message using extension - skip it
	// if message was already confirmed - skip it
	if !ok || state == EnvelopeSent {
		return
	}
	log.Debug("envelope is sent", "hash", event.Hash, "peer", event.Peer)
	if event.Batch != (common.Hash{}) {
		if _, ok := m.batches[event.Batch]; !ok {
			m.batches[event.Batch] = map[common.Hash]struct{}{}
		}
		m.batches[event.Batch][event.Hash] = struct{}{}
		log.Debug("waiting for a confirmation", "batch", event.Batch)
	} else {
		m.envelopes[event.Hash] = EnvelopeSent
		if m.handler != nil {
			mID := messageID(m.messages[event.Hash])
			m.handler.EnvelopeSent(mID)
		}
	}
}

func (m *EnvelopesMonitor) isMailserver(peer enode.ID) bool {
	return m.mailPeers.Exist(peer)
}

func (m *EnvelopesMonitor) handleAcknowledgedBatch(event whisper.EnvelopeEvent) {
	if m.mailServerConfirmation {
		if !m.isMailserver(event.Peer) {
			return
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	envelopes, ok := m.batches[event.Batch]
	if !ok {
		log.Debug("batch is not found", "batch", event.Batch)
	}
	log.Debug("received a confirmation", "batch", event.Batch, "peer", event.Peer)
	for hash := range envelopes {
		state, ok := m.envelopes[hash]
		if !ok || state == EnvelopeSent {
			continue
		}
		m.envelopes[hash] = EnvelopeSent
		if m.handler != nil {
			mID := messageID(m.messages[hash])
			m.handler.EnvelopeSent(mID)
		}
	}
	delete(m.batches, event.Batch)
}

func (m *EnvelopesMonitor) handleEventEnvelopeExpired(event whisper.EnvelopeEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.envelopes[event.Hash]; ok {
		delete(m.envelopes, event.Hash)
		if state == EnvelopeSent {
			delete(m.messageToEnvelope, messageID(m.messages[event.Hash]))
			delete(m.messages, event.Hash)
			delete(m.attempts, event.Hash)
			return
		}
		message, exist := m.messages[event.Hash]
		if !exist {
			log.Error("message was deleted erroneously", "envelope hash", event.Hash)
		}
		mID := messageID(message)
		attempt := m.attempts[event.Hash]
		if attempt < maxAttempts {
			log.Debug("retrying to send a message", "message id", mID, "attempt", attempt+1)
			hex, err := m.whisperAPI.Post(context.TODO(), message)
			envelopeID := common.BytesToHash(hex)
			if err != nil {
				log.Error("failed to retry sending message", "message id", mID, "attempt", attempt+1)
			}
			m.messageToEnvelope[mID] = envelopeID
			m.envelopes[envelopeID] = EnvelopePosted
			m.messages[envelopeID] = message
			m.attempts[envelopeID] = attempt + 1
		} else {
			delete(m.messageToEnvelope, messageID(m.messages[event.Hash]))
			delete(m.messages, event.Hash)
			delete(m.attempts, event.Hash)
			log.Debug("envelope expired", "hash", event.Hash, "state", state)
			if m.handler != nil {
				m.handler.EnvelopeExpired(mID)
			}
		}
	}
}

func (m *EnvelopesMonitor) handleEventEnvelopeReceived(event whisper.EnvelopeEvent) {
	if m.mailServerConfirmation {
		if !m.isMailserver(event.Peer) {
			return
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.envelopes[event.Hash]
	if !ok || state != EnvelopePosted {
		return
	}
	log.Debug("expected envelope received", "hash", event.Hash, "peer", event.Peer)
	delete(m.envelopes, event.Hash)
	mID := messageID(m.messages[event.Hash])
	delete(m.messageToEnvelope, mID)
	delete(m.messages, event.Hash)
	delete(m.attempts, event.Hash)
	if m.handler != nil {
		m.handler.EnvelopeSent(mID)
	}
}
