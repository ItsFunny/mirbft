/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package statemachine

import (
	"fmt"

	pb "github.com/IBM/mirbft/mirbftpb"
	"github.com/IBM/mirbft/pkg/status"
)

// bucketID is the identifier for a bucket.  It is a simple alias to a uint64, but
// is used to help disambiguate function signatures which accept multiple uint64
// values with different meanings.
type bucketID uint64

// nodeID represents the identifier assigned to a node.  It is a simple alias to a uint64, but
// is used to help disambiguate function signatures which accept multiple uint64
// values with different meanings.
type nodeID uint64

type stateMachineState int

func assertFailed(failure, format string, args ...interface{}) {
	panic(
		fmt.Sprintf(
			fmt.Sprintf("assertion failed, code bug? -- %s -- %s", failure, format),
			args...,
		),
	)
}

func assertTrue(value bool, text string) {
	assertTruef(value, text)
}

func assertTruef(value bool, format string, args ...interface{}) {
	if !value {
		assertFailed("expected false to be true", format, args...)
	}
}

func assertEqual(lhs, rhs interface{}, text string) {
	assertEqualf(lhs, rhs, text)
}

func assertEqualf(lhs, rhs interface{}, format string, args ...interface{}) {
	if lhs != rhs {
		assertFailed(fmt.Sprintf("expected %v == %v", lhs, rhs), format, args...)
	}
}

func assertNotEqual(lhs, rhs interface{}, text string) {
	assertNotEqualf(lhs, rhs, text)
}

func assertNotEqualf(lhs, rhs interface{}, format string, args ...interface{}) {
	if lhs == rhs {
		assertFailed(fmt.Sprintf("expected %v != %v", lhs, rhs), format, args...)
	}
}

func assertGreaterThanOrEqual(lhs, rhs uint64, text string) {
	assertGreaterThanOrEqualf(lhs, rhs, text)
}

func assertGreaterThanOrEqualf(lhs, rhs uint64, format string, args ...interface{}) {
	if lhs < rhs {
		assertFailed(fmt.Sprintf("expected %v >= %v", lhs, rhs), format, args...)
	}
}

func assertGreaterThan(lhs, rhs uint64, text string) {
	assertGreaterThanf(lhs, rhs, text)
}

func assertGreaterThanf(lhs, rhs uint64, format string, args ...interface{}) {
	if lhs <= rhs {
		assertFailed(fmt.Sprintf("expected %v > %v", lhs, rhs), format, args...)
	}
}

const (
	smUninitialized stateMachineState = iota
	smLoadingPersisted
	smInitialized
)

// StateMachine contains a deterministic processor for mirbftpb state events.
// This structure should almost never be initialized directly but should instead
// be allocated via StartNode.
type StateMachine struct {
	Logger Logger

	state stateMachineState

	myConfig               *pb.StateEvent_InitialParameters
	commitState            *commitState
	clientTracker          *clientTracker
	clientHashDisseminator *clientHashDisseminator

	nodeBuffers       *nodeBuffers
	batchTracker      *batchTracker
	checkpointTracker *checkpointTracker
	epochTracker      *epochTracker
	persisted         *persisted
}

func (sm *StateMachine) initialize(parameters *pb.StateEvent_InitialParameters) {
	assertEqualf(sm.state, smUninitialized, "state machine has already been initialized")

	sm.myConfig = parameters
	sm.state = smLoadingPersisted
	sm.persisted = newPersisted(sm.Logger)

	// we use a dummy initial state for components to allow us to use
	// a common 'reconfiguration'/'state transfer' path for initialization.
	dummyInitialState := &pb.NetworkState{
		Config: &pb.NetworkState_Config{
			Nodes:              []uint64{sm.myConfig.Id},
			MaxEpochLength:     1,
			CheckpointInterval: 1,
			NumberOfBuckets:    1,
		},
	}

	sm.nodeBuffers = newNodeBuffers(sm.myConfig, sm.Logger)
	sm.checkpointTracker = newCheckpointTracker(0, dummyInitialState, sm.persisted, sm.nodeBuffers, sm.myConfig, sm.Logger)
	sm.clientTracker = newClientTracker(sm.myConfig, sm.Logger)
	sm.commitState = newCommitState(sm.persisted, sm.Logger)
	sm.clientHashDisseminator = newClientHashDisseminator(sm.nodeBuffers, sm.myConfig, sm.Logger, sm.clientTracker)
	sm.batchTracker = newBatchTracker(sm.persisted)
	sm.epochTracker = newEpochTracker(
		sm.persisted,
		sm.nodeBuffers,
		sm.commitState,
		dummyInitialState.Config,
		sm.Logger,
		sm.myConfig,
		sm.batchTracker,
		sm.clientTracker,
		sm.clientHashDisseminator,
	)

}

func (sm *StateMachine) applyPersisted(index uint64, data *pb.Persistent) {
	assertEqualf(sm.state, smLoadingPersisted, "state machine has already finished loading persisted data")
	sm.persisted.appendInitialLoad(index, data)
}

func (sm *StateMachine) completeInitialization() *actionSet {
	assertEqualf(sm.state, smLoadingPersisted, "state machine has already finished loading persisted data")

	sm.state = smInitialized

	return sm.reinitialize()
}

func (sm *StateMachine) ApplyEvent(stateEvent *pb.StateEvent) *pb.StateEventResult {
	return &(sm.applyEvent(stateEvent).StateEventResult)
}

func (sm *StateMachine) applyEvent(stateEvent *pb.StateEvent) *actionSet {
	assertInitialized := func() {
		assertEqualf(sm.state, smInitialized, "cannot apply events to an uninitialized state machine")
	}

	actions := &actionSet{}

	switch event := stateEvent.Type.(type) {
	case *pb.StateEvent_Initialize:
		sm.initialize(event.Initialize)
		return &actionSet{}
	case *pb.StateEvent_LoadEntry:
		sm.applyPersisted(event.LoadEntry.Index, event.LoadEntry.Data)
		return &actionSet{}
	case *pb.StateEvent_CompleteInitialization:
		return sm.completeInitialization()
	case *pb.StateEvent_Tick:
		assertInitialized()
		actions.concat(sm.clientHashDisseminator.tick())
		actions.concat(sm.epochTracker.tick())
	case *pb.StateEvent_Step:
		assertInitialized()
		actions.concat(sm.step(
			nodeID(event.Step.Source),
			event.Step.Msg,
		))
	case *pb.StateEvent_AddResults:
		assertInitialized()
		actions.concat(sm.processResults(
			event.AddResults,
		))
	case *pb.StateEvent_AddClientResults:
		assertInitialized()
		actions.concat(sm.clientHashDisseminator.applyNewRequests(
			event.AddClientResults.Persisted,
		))
	case *pb.StateEvent_Transfer:
		assertEqualf(sm.commitState.transferring, true, "state transfer event received but the state machine did not request transfer")

		sm.Logger.Log(LevelDebug, "state transfer completed", "seq_no", event.Transfer.SeqNo)

		actions.concat(sm.persisted.addCEntry(event.Transfer))
		actions.concat(sm.reinitialize())
	case *pb.StateEvent_ActionsReceived:
		// This is a bit odd, in that it's a no-op, but it's harmless
		// and allows for much more insightful playback events (allowing
		// us to tie action results to a particular set of actions)
		return &actionSet{}
	case *pb.StateEvent_ClientActionsReceived:
		// This is exactly like ActionsReceived, a no-op for audit.
		return &actionSet{}
	default:
		panic(fmt.Sprintf("unknown state event type: %T", stateEvent.Type))
	}

	// A nice guarantee we have, is that for any given event, at most, one watermark movement is
	// required.  It is not possible for the watermarks to move twice, as it would require
	// new checkpoint messages from ourselves, and because of reconfiguration, we can only generate
	// a checkpoint request after the previous checkpoint requests has been returned (because
	// the checkpoint result includes any pending reconfiguration which must be reflected in
	// the next checkpoint.)
	if sm.checkpointTracker.state == cpsGarbageCollectable {
		newLow := sm.checkpointTracker.garbageCollect()
		sm.Logger.Log(LevelDebug, "garbage collecting through", "seq_no", newLow)

		sm.persisted.truncate(newLow)

		if newLow > uint64(sm.checkpointTracker.networkConfig.CheckpointInterval) {
			// Note, we leave an extra checkpoint worth of batches around, to help
			// during epoch change.
			sm.batchTracker.truncate(newLow - uint64(sm.checkpointTracker.networkConfig.CheckpointInterval))
		}
		actions.concat(sm.epochTracker.moveLowWatermark(newLow))
	}

	for {
		// We note all of the commits that occured in response to the current event
		// as well as any watermark movement.  Then, based on this information we
		// may continue to iterate the state machine, and do so, so long as
		// attempting to advance the state causes new actions.

		actions.concat(&actionSet{
			StateEventResult: pb.StateEventResult{
				Commits: sm.commitState.drain(),
			},
		})

		loopActions := sm.epochTracker.advanceState()
		if loopActions.isEmpty() {
			break
		}

		actions.concat(loopActions)
	}

	return actions
}

// reinitialize causes the components to reinitialize themselves from the logs.
// varying from component to component, useful state will be retained.  For instance,
// the clientTracker retains in-window ACKs for still-extant clients.  The checkpointTracker
// retains checkpoint messages sent by other replicas, etc.
func (sm *StateMachine) reinitialize() *actionSet {
	defer sm.Logger.Log(LevelInfo, "state machine reinitialized (either due to start, state transfer, or reconfiguration)")

	actions := sm.recoverLog()
	actions.concat(sm.commitState.reinitialize())
	sm.clientTracker.reinitialize(sm.commitState.activeState)
	actions.concat(sm.clientHashDisseminator.reinitialize(sm.commitState.lowWatermark, sm.commitState.activeState))

	sm.checkpointTracker.reinitialize()
	sm.batchTracker.reinitialize()
	return actions.concat(sm.epochTracker.reinitialize())
}

func (sm *StateMachine) recoverLog() *actionSet {
	var lastCEntry *pb.CEntry

	actions := &actionSet{}

	sm.persisted.iterate(logIterator{
		onCEntry: func(cEntry *pb.CEntry) {
			lastCEntry = cEntry
		},
		onFEntry: func(fEntry *pb.FEntry) {
			assertNotEqualf(lastCEntry, nil, "FEntry without corresponding CEntry, log is corrupt")
			actions.concat(sm.persisted.truncate(lastCEntry.SeqNo))
		},
	})

	assertNotEqualf(lastCEntry, nil, "found no checkpoints in the log")

	return actions
}

func (sm *StateMachine) step(source nodeID, msg *pb.Msg) *actionSet {
	actions := &actionSet{}
	switch msg.Type.(type) {
	case *pb.Msg_RequestAck:
		return actions.concat(sm.clientHashDisseminator.step(source, msg))
	case *pb.Msg_FetchRequest:
		return actions.concat(sm.clientHashDisseminator.step(source, msg))
	case *pb.Msg_ForwardRequest:
		return actions.concat(sm.clientHashDisseminator.step(source, msg))
	case *pb.Msg_Checkpoint:
		sm.checkpointTracker.step(source, msg)
		return &actionSet{}
	case *pb.Msg_FetchBatch:
		// TODO decide if we want some buffering?
		return sm.batchTracker.step(source, msg)
	case *pb.Msg_ForwardBatch:
		// TODO decide if we want some buffering?
		return sm.batchTracker.step(source, msg)
	case *pb.Msg_Suspect:
		return sm.epochTracker.step(source, msg)
	case *pb.Msg_EpochChange:
		return sm.epochTracker.step(source, msg)
	case *pb.Msg_EpochChangeAck:
		return sm.epochTracker.step(source, msg)
	case *pb.Msg_NewEpoch:
		return sm.epochTracker.step(source, msg)
	case *pb.Msg_NewEpochEcho:
		return sm.epochTracker.step(source, msg)
	case *pb.Msg_NewEpochReady:
		return sm.epochTracker.step(source, msg)
	case *pb.Msg_Preprepare:
		return sm.epochTracker.step(source, msg)
	case *pb.Msg_Prepare:
		return sm.epochTracker.step(source, msg)
	case *pb.Msg_Commit:
		return sm.epochTracker.step(source, msg)
	default:
		panic(fmt.Sprintf("unexpected bad message type %T", msg.Type))
	}
}

func (sm *StateMachine) processResults(results *pb.StateEvent_ActionResults) *actionSet {
	actions := &actionSet{}

	for _, checkpointResult := range results.Checkpoints {
		if checkpointResult.SeqNo < sm.commitState.lowWatermark {
			// Sometimes the application might send a stale checkpoint after
			// state transfer, so we ignore.
			continue
		}

		expectedSeqNo := sm.commitState.lowWatermark + uint64(sm.commitState.activeState.Config.CheckpointInterval)
		assertEqual(expectedSeqNo, checkpointResult.SeqNo, "new checkpoint results muts be exactly one checkpoint interval after the last")

		var epochConfig *pb.EpochConfig
		if sm.epochTracker.currentEpoch.activeEpoch != nil {
			// Of course this means epochConfig may be nil, and that's okay
			// since we know that no new pEntries/qEntries can be persisted
			// until we send an epoch related persisted entry
			epochConfig = sm.epochTracker.currentEpoch.activeEpoch.epochConfig
		}

		prevStopAtSeqNo := sm.commitState.stopAtSeqNo
		actions.concat(sm.commitState.applyCheckpointResult(epochConfig, checkpointResult))
		if prevStopAtSeqNo < sm.commitState.stopAtSeqNo {
			sm.clientTracker.allocate(checkpointResult.SeqNo, checkpointResult.NetworkState)
			actions.concat(sm.clientHashDisseminator.allocate(checkpointResult.SeqNo, checkpointResult.NetworkState))
		}
	}

	for _, hashResult := range results.Digests {
		switch hashType := hashResult.Type.(type) {
		case *pb.HashResult_Batch_:
			batch := hashType.Batch
			sm.batchTracker.addBatch(batch.SeqNo, hashResult.Digest, batch.RequestAcks)
			actions.concat(sm.epochTracker.applyBatchHashResult(batch.Epoch, batch.SeqNo, hashResult.Digest))
		case *pb.HashResult_EpochChange_:
			epochChange := hashType.EpochChange
			actions.concat(sm.epochTracker.applyEpochChangeDigest(epochChange, hashResult.Digest))
		case *pb.HashResult_VerifyBatch_:
			verifyBatch := hashType.VerifyBatch
			sm.batchTracker.applyVerifyBatchHashResult(hashResult.Digest, verifyBatch)
			if !sm.batchTracker.hasFetchInFlight() && sm.epochTracker.currentEpoch.state == etFetching {
				actions.concat(sm.epochTracker.currentEpoch.fetchNewEpochState())
			}
		default:
			panic("no hash result type set")
		}
	}

	return actions
}

func (sm *StateMachine) Status() *status.StateMachine {
	if sm.state != smInitialized {
		return &status.StateMachine{}
	}

	clientTrackerStatus := make([]*status.ClientTracker, len(sm.clientTracker.clientStates))

	for i, clientState := range sm.clientTracker.clientStates {
		clientTrackerStatus[i] = sm.clientHashDisseminator.clients[clientState.Id].status()
	}

	nodes := make([]*status.NodeBuffer, len(sm.checkpointTracker.networkConfig.Nodes))
	for i, id := range sm.checkpointTracker.networkConfig.Nodes {
		nodes[i] = &status.NodeBuffer{
			ID: id,
		} // TODO, actually populate this again
	}

	lowWatermark, highWatermark, bucketStatus := sm.epochTracker.currentEpoch.bucketStatus()

	checkpoints := sm.checkpointTracker.status()

	return &status.StateMachine{
		NodeID:        sm.myConfig.Id,
		LowWatermark:  lowWatermark,
		HighWatermark: highWatermark,
		EpochTracker:  sm.epochTracker.status(),
		ClientWindows: clientTrackerStatus,
		Buckets:       bucketStatus,
		Checkpoints:   checkpoints,
		NodeBuffers:   nodes,
	}
}
