package reactor

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/certusone/wormhole/node/pkg/p2p"

	"github.com/certusone/wormhole/node/pkg/supervisor"

	node_common "github.com/certusone/wormhole/node/pkg/common"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"go.uber.org/zap"

	gossipv1 "github.com/certusone/wormhole/node/pkg/proto/gossip/v1"
	"github.com/wormhole-foundation/wormhole/sdk/vaa"
)

// ConsensusReactor implements the full consensus processor for a single Observation. It cannot be reused after being
// finalized.
type (
	ConsensusReactor[K Observation] struct {
		// group is the name of the reactor group
		group string

		// Copy of the guardian set valid at observation/injection time.
		gs *node_common.GuardianSet

		// Channel for receiving local observations
		observationChannel chan K
		// Channel for receiving foreign observations
		foreignObservationChannel chan *gossipv1.SignedObservation

		// consensusSender is an interface to send local signatures to the network
		consensusSender p2p.GossipSender

		// Hook to be called on a state transition
		stateTransitionHook StateTransitionHook[K]
		// Configuration of the reactor
		config Config

		// reactorState holds all mutable fields of the reactor. They may only be used while holding the
		// reactorState.mutex.
		state reactorState[K]

		logger *zap.Logger
	}

	reactorState[K Observation] struct {
		// Current state of the reactor
		currentState State
		// First time this digest was seen (possibly even before we observed it ourselves).
		firstSeen time.Time
		// Time of the last new observation received
		lastObservation time.Time
		// The most recent time that the signature / observation has been transmitted
		lastTransmission time.Time
		// Time quorum was reached
		timeQuorum time.Time

		// Copy of our observation.
		observation K
		// Map of signatures seen by guardian. During guardian set updates, this may contain signatures belonging
		// to either the old or new guardian set.
		signatures map[ethcommon.Address][]byte
		// Copy of the bytes we submitted (ourObservation, but signed and serialized). Used for retransmissions.
		localSignature []byte

		mutex sync.Mutex
	}
)

// StateTransitionHook is a hook for handling state transitions. It is called as a goroutine after the state transition
// is completed.
type StateTransitionHook[K Observation] func(reactor *ConsensusReactor[K], oldState, newState State)

// Config allows to parametrize the consensus reactor
type Config struct {
	// RetransmitFrequency is the frequency of observation rebroadcasts
	RetransmitFrequency time.Duration
	// QuorumGracePeriod is the time to wait for more signatures after quorum before finalizing the reactor
	QuorumGracePeriod time.Duration
	// QuorumTimeout is the time to wait for quorum before finalizing the reactor
	QuorumTimeout time.Duration
	// UnobservedTimeout is the time to wait for either a completed VAA or a local observation before finalizing the
	// reactor after only having received remote observations.
	UnobservedTimeout time.Duration
	// Signer to use for local observations. If Signer is nil, the reactor will not participate in consensus.
	Signer Signer
}

// Observation defines the interface for any message observed by the guardian.
type Observation interface {
	// MessageID returns a human-readable message-id.
	MessageID() string
	// SigningMsg returns the hash of the signing body of the message. This is used
	// for signature generation and verification.
	SigningMsg() ethcommon.Hash
}

// State of the reactor
type State string

const (
	// StateInitialized is used for a freshly created reactor. A reactor in the StateInitialized will wait for either
	// a local or foreign observation.
	StateInitialized State = "initialized"
	// StateObserved indicates that the reactor has seen =1 local observation and >= 0 foreign observations. It is able
	// to produce a full VAA upon reaching quorum.
	StateObserved State = "observed"
	// StateUnobserved indicates that the reactor has seen >= 1 foreign observations but no local observation. It is not
	// able to produce a full VAA without a local observation.
	StateUnobserved State = "unobserved"
	// StateQuorum indicates that the reactor has seen a local observation and a quorum of foreign observations. It has
	// all data to produce a full VAA.
	StateQuorum State = "quorum"
	// StateQuorumUnobserved indicates that the reactor has seen a quorum of foreign observations but no local observation.
	// It can only produce a full VAA after receiving a local observation.
	StateQuorumUnobserved State = "quorum_unobserved"
	// StateFinalized is a reactor that has gone through a full lifecycle. It holds all information required to produce
	// a full VAA.
	StateFinalized State = "finalized"
	// StateTimedOut is a reactor that has gone through a full lifecycle. It did not manage to achieve locally confirmed
	// quorum (i.e. both a local observation and quorum) within the configured timeouts.
	StateTimedOut State = "timed_out"
)

func NewReactor[K Observation](group string, config Config, gs *node_common.GuardianSet, consensusSender p2p.GossipSender, s StateTransitionHook[K]) *ConsensusReactor[K] {
	c := &ConsensusReactor[K]{
		group: group,
		state: reactorState[K]{
			currentState: StateInitialized,
			signatures:   map[ethcommon.Address][]byte{},
		},
		consensusSender:           consensusSender,
		gs:                        gs,
		stateTransitionHook:       s,
		config:                    config,
		foreignObservationChannel: make(chan *gossipv1.SignedObservation, 10),
		observationChannel:        make(chan K, 10),
	}

	return c
}

func (c *ConsensusReactor[K]) Run(ctx context.Context) error {
	c.logger = supervisor.Logger(ctx)
	c.logger.With(zap.String("group", c.group))

	supervisor.Signal(ctx, supervisor.SignalHealthy)

	if c.State() == StateFinalized {
		supervisor.Signal(ctx, supervisor.SignalDone)
		return nil
	}

	tickFrequency := time.Second
	ticker := time.NewTicker(tickFrequency)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case o := <-c.observationChannel:
			c.observed(ctx, o)
		case o := <-c.foreignObservationChannel:
			c.observationReceived(ctx, o)
		case <-ticker.C:
			terminate, err := c.housekeeping(ctx)
			if err != nil {
				c.logger.Warn("housekeeping failed", zap.Error(err))
			}
			if terminate {
				c.logger.Debug("reactor concluded; shutting down processing loop")
				// Signal done such that the routine does not restart
				// TODO this might leave garbage in the supervisor tree
				supervisor.Signal(ctx, supervisor.SignalDone)
				return nil
			}
		}
	}
}

func (c *ConsensusReactor[K]) ObservationChannel() chan<- K {
	return c.observationChannel
}

func (c *ConsensusReactor[K]) ForeignObservationChannel() chan<- *gossipv1.SignedObservation {
	return c.foreignObservationChannel
}

// State returns the current state of the reactor
func (c *ConsensusReactor[K]) State() State {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()
	return c.state.currentState
}

// Observation returns the current observation stored in the reactor.
func (c *ConsensusReactor[K]) Observation() K {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()
	return c.state.observation
}

// HasQuorum returns whether the reactor holds a quorum of signatures.
func (c *ConsensusReactor[K]) HasQuorum() bool {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()
	return c.hasQuorum()
}

// LastObservation the time the last signed observation was received.
func (c *ConsensusReactor[K]) LastObservation() time.Time {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()
	return c.state.lastObservation
}

// VAASignatures returns the stored signatures in the order required by a VAA.
func (c *ConsensusReactor[K]) VAASignatures() []*vaa.Signature {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()
	var sigs []*vaa.Signature
	for i, a := range c.gs.Keys {
		s, ok := c.state.signatures[a]

		if ok {
			var bs [65]byte
			if n := copy(bs[:], s); n != 65 {
				panic(fmt.Sprintf("invalid sig len: %d", n))
			}

			sigs = append(sigs, &vaa.Signature{
				Index:     uint8(i),
				Signature: bs,
			})
		}
	}
	return sigs
}

func (c *ConsensusReactor[K]) housekeeping(ctx context.Context) (bool, error) {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()
	defer c.logger.Debug("housekeeping completed")

	c.logger.Debug("starting housekeeping")

	switch c.state.currentState {
	case StateUnobserved:
		if time.Since(c.state.firstSeen) > c.config.UnobservedTimeout {
			c.logger.Debug("timing out", zap.String("reason", "unobserved_timeout"))
			// Time out
			c.timeOut()
		}
	case StateObserved:
		if time.Since(c.state.lastObservation) > c.config.QuorumTimeout {
			c.logger.Debug("timing out", zap.String("reason", "quorum_timeout"))
			// Time out
			c.timeOut()
		}

		if time.Since(c.state.lastTransmission) > c.config.RetransmitFrequency {
			// TODO backoff when transmission fails
			c.logger.Debug("retransmitting")
			reactorResubmission.WithLabelValues(c.group).Inc()
			// Retransmit signature
			err := c.transmitSignature(ctx)
			if err != nil {
				c.logger.Error("failed to retransmit signature", zap.Error(err))
			}
		}
	case StateQuorum:
		if time.Since(c.state.timeQuorum) > c.config.QuorumGracePeriod || len(c.gs.Keys) == len(c.state.signatures) {
			c.logger.Debug("timing out", zap.String("reason", "quorum_grace"))
			// Time out
			c.timeOut()
		}
	case StateQuorumUnobserved:
		if time.Since(c.state.firstSeen) > c.config.UnobservedTimeout {
			c.logger.Debug("timing out", zap.String("reason", "quorum_unobserved_timeout"))
			// Time out
			c.timeOut()
		}
	case StateFinalized, StateTimedOut:
		// This is the final iteration. Do cleanup
		return true, nil
	}

	return false, nil
}

func (c *ConsensusReactor[K]) observationReceived(ctx context.Context, m *gossipv1.SignedObservation) {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()

	observationsReceivedTotal.WithLabelValues(c.group).Inc()

	c.logger.Debug("received foreign observation", zap.Any("observation", m))

	if c.state.currentState != StateObserved && c.state.currentState != StateUnobserved && c.state.currentState != StateQuorum && c.state.currentState != StateQuorumUnobserved && c.state.currentState != StateInitialized {
		return
	}

	hash := hex.EncodeToString(m.Hash)

	// Hooray! Now, we have verified all fields on SignedObservation and know that it includes
	// a valid signature by an active guardian. We still don't fully trust them, as they may be
	// byzantine, but now we know who we're dealing with.
	err := verifySignedObservation(c.group, m, c.gs)
	if err != nil {
		c.logger.Info("failed to verify signed observation",
			zap.Error(err),
			zap.String("digest", hash),
			zap.String("signature", hex.EncodeToString(m.Signature)),
			zap.String("addr", hex.EncodeToString(m.Addr)),
		)
		return
	}
	theirAddr := ethcommon.BytesToAddress(m.Addr)
	observationsReceivedByGuardianAddressTotal.WithLabelValues(c.group, theirAddr.Hex()).Inc()

	c.logger.Debug("accepted foreign observation", zap.Any("observation", m), zap.Stringer("address", theirAddr))

	// Have we already received this observation
	if _, has := c.state.signatures[theirAddr]; has {
		// TODO log duplicate
		return
	}

	// Store their signature
	c.state.signatures[theirAddr] = m.Signature
	c.state.lastObservation = time.Now()

	if c.state.currentState == StateInitialized {
		c.logger.Debug("received observation before own observation", zap.Any("observation", m))
		c.state.firstSeen = time.Now()
		c.stateTransition(StateUnobserved)
	}

	// If we haven't reached quorum yet, there is nothing more to do
	if !c.hasQuorum() {
		return
	}

	// Transition to quorum states
	switch c.state.currentState {
	case StateObserved:
		reactorQuorum.WithLabelValues(c.group, "quorum").Inc()
		c.stateTransition(StateQuorum)
	case StateUnobserved:
		reactorQuorum.WithLabelValues(c.group, "quorum_unobserved").Inc()
		c.stateTransition(StateQuorumUnobserved)
	}
}

func (c *ConsensusReactor[K]) observed(ctx context.Context, o K) {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()

	c.logger.Debug("observed", zap.Any("observation", o))

	if c.state.currentState != StateInitialized && c.state.currentState != StateUnobserved && c.state.currentState != StateQuorumUnobserved {
		return
	}

	// Late observation
	if c.state.currentState == StateQuorumUnobserved {
		reactorObservedLate.WithLabelValues(c.group).Inc()
	}

	c.state.observation = o

	if c.config.Signer != nil {
		// Generate digest of the unsigned VAA.
		digest := o.SigningMsg()

		// Sign the digest using our node's guardian key.
		timeout, cancel := context.WithTimeout(ctx, time.Second*5)
		defer cancel()
		localAddr, err := c.config.Signer.Address(timeout)
		if err != nil {
			panic(err)
		}
		s, err := c.config.Signer.Sign(timeout, digest.Bytes())
		if err != nil {
			panic(err)
		}
		c.state.localSignature = s

		messagesSignedTotal.WithLabelValues(c.group).Inc()

		// Store in signatures array
		c.state.signatures[localAddr] = s

		err = c.transmitSignature(ctx)
		if err != nil {
			c.logger.Error("failed to transmit signature on observation", zap.Error(err))
		}

	}

	// Transition to quorum states
	switch c.state.currentState {
	case StateInitialized:
		c.state.firstSeen = time.Now()
		c.state.lastObservation = time.Now()
		c.stateTransition(StateObserved)
	case StateUnobserved:
		c.state.lastObservation = time.Now()
		c.stateTransition(StateObserved)
	case StateQuorumUnobserved:
		reactorQuorum.WithLabelValues(c.group, "quorum").Inc()
		c.stateTransition(StateQuorum)
		return
	}

	// If we haven't reached quorum in this event, there is nothing more to do
	if !c.hasQuorum() {
		return
	}

	// We immediately reached quorum
	switch c.state.currentState {
	case StateObserved:
		reactorQuorum.WithLabelValues(c.group, "quorum").Inc()
		c.stateTransition(StateQuorum)
	}
}

// timeOut triggers the timeout state transition. It must only be called while holding the reactorState.mutex.
func (c *ConsensusReactor[K]) timeOut() {
	if c.state.currentState == StateQuorum {
		reactorFinalized.WithLabelValues(c.group).Inc()
		c.stateTransition(StateFinalized)
	} else {
		reactorTimedOut.WithLabelValues(c.group, string(c.state.currentState)).Inc()
		c.stateTransition(StateTimedOut)
	}
}

// stateTransition updates the state and triggers the hook. It must only be called while holding the reactorState.mutex.
func (c *ConsensusReactor[K]) stateTransition(to State) {
	c.logger.Debug("state transition", zap.String("from", string(c.state.currentState)), zap.String("to", string(to)))
	previousState := c.state.currentState
	c.state.currentState = to

	if c.stateTransitionHook != nil {
		go c.stateTransitionHook(c, previousState, to)
	}
}

// hasQuorum returns whether the reactor holds a quorum of signatures. It must only be called while holding reactorState.mutex.
func (c *ConsensusReactor[K]) hasQuorum() bool {
	return len(c.state.signatures) >= vaa.CalculateQuorum(len(c.gs.Keys))
}

// transmitSignature broadcasts the localSignature of the reactor. It must only be called while holding stateMutex.
func (c *ConsensusReactor[K]) transmitSignature(ctx context.Context) error {
	if c.config.Signer == nil {
		return fmt.Errorf("can't broadcast signature without signer")
	}
	if c.consensusSender == nil {
		return fmt.Errorf("can't broadcast signature without consensus sender")
	}

	timeout, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	addr, err := c.config.Signer.Address(timeout)
	if err != nil {
		return fmt.Errorf("failed to get signer address for signature broadcast: %w", err)
	}
	signedO := &gossipv1.SignedObservation{
		Addr:      addr.Bytes(),
		Hash:      c.state.observation.SigningMsg().Bytes(),
		Signature: c.state.localSignature,
		TxHash:    []byte{},
		MessageId: c.state.observation.MessageID(),
	}
	timeout, cancel = context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	err = c.consensusSender.Send(timeout, &gossipv1.GossipMessage{Message: &gossipv1.GossipMessage_SignedObservation{SignedObservation: signedO}})
	if err != nil {
		return fmt.Errorf("failed to broadcast observation: %w", err)
	}

	observationsBroadcastTotal.WithLabelValues(c.group).Inc()
	c.state.lastTransmission = time.Now()

	return nil
}

func verifySignedObservation(group string, m *gossipv1.SignedObservation, gs *node_common.GuardianSet) error {
	// Verify the Guardian's signature. This verifies that m.Signature matches m.Hash and recovers
	// the public key that was used to sign the payload.
	pk, err := crypto.Ecrecover(m.Hash, m.Signature)
	if err != nil {
		observationsFailedTotal.WithLabelValues(group, "invalid_signature").Inc()
		return fmt.Errorf("failed to verify signature: %w", err)
	}

	// Verify that m.Addr matches the public key that signed m.Hash.
	theirAddr := ethcommon.BytesToAddress(m.Addr)
	sigPub := ethcommon.BytesToAddress(crypto.Keccak256(pk[1:])[12:])

	if theirAddr != sigPub {
		observationsFailedTotal.WithLabelValues(group, "pubkey_mismatch").Inc()
		return fmt.Errorf("address does not match pubkey: %s != %s", theirAddr.Hex(), sigPub.Hex())
	}

	// Verify that m.Addr is included in the guardian set. If it's not, drop the message. In case it's us
	// who have the outdated guardian set, we'll just wait for the message to be retransmitted eventually.
	_, ok := gs.KeyIndex(theirAddr)
	if !ok {
		observationsFailedTotal.WithLabelValues(group, "unknown_guardian").Inc()
		return fmt.Errorf("unknown guardian: %s", theirAddr.Hex())
	}

	return nil
}
