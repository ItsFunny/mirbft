/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package statemachine

import (
	"fmt"

	pb "github.com/IBM/mirbft/mirbftpb"
)

type logIterator struct {
	onQEntry   func(*pb.QEntry)
	onPEntry   func(*pb.PEntry)
	onCEntry   func(*pb.CEntry)
	onNEntry   func(*pb.NEntry)
	onFEntry   func(*pb.FEntry)
	onECEntry  func(*pb.ECEntry)
	onTEntry   func(*pb.TEntry)
	onSuspect  func(*pb.Suspect)
	shouldExit func() bool
	// TODO, suspect_ready
}

type logEntry struct {
	index uint64
	entry *pb.Persistent
	next  *logEntry
}

type persisted struct {
	nextIndex uint64
	logHead   *logEntry
	logTail   *logEntry

	logger Logger
}

func newPersisted(logger Logger) *persisted {
	return &persisted{
		logger: logger,
	}
}

func (p *persisted) appendInitialLoad(index uint64, data *pb.Persistent) {
	if p.logHead == nil {
		p.nextIndex = index
		p.logHead = &logEntry{
			index: index,
			entry: data,
		}
		p.logTail = p.logHead
	} else {
		p.logTail.next = &logEntry{
			index: index,
			entry: data,
		}
		p.logTail = p.logTail.next
	}
	if p.nextIndex != index {
		panic(fmt.Sprintf("WAL indexes out of order! Expected %d got %d, was your WAL corrupted?", p.nextIndex, index))
	}
	p.nextIndex = index + 1
}

func (p *persisted) appendLogEntry(entry *pb.Persistent) *actionSet {
	p.logTail.next = &logEntry{
		index: p.nextIndex,
		entry: entry,
	}
	p.logTail = p.logTail.next
	result := (&actionSet{}).persist(p.nextIndex, entry)
	p.nextIndex++
	return result
}

func (p *persisted) addPEntry(pEntry *pb.PEntry) *actionSet {
	d := &pb.Persistent{
		Type: &pb.Persistent_PEntry{
			PEntry: pEntry,
		},
	}

	return p.appendLogEntry(d)
}

func (p *persisted) addQEntry(qEntry *pb.QEntry) *actionSet {
	d := &pb.Persistent{
		Type: &pb.Persistent_QEntry{
			QEntry: qEntry,
		},
	}

	return p.appendLogEntry(d)
}

func (p *persisted) addNEntry(nEntry *pb.NEntry) *actionSet {
	d := &pb.Persistent{
		Type: &pb.Persistent_NEntry{
			NEntry: nEntry,
		},
	}

	return p.appendLogEntry(d)
}

func (p *persisted) addCEntry(cEntry *pb.CEntry) *actionSet {
	assertNotEqual(cEntry.NetworkState, nil, "network config must be set")

	d := &pb.Persistent{
		Type: &pb.Persistent_CEntry{
			CEntry: cEntry,
		},
	}

	return p.appendLogEntry(d)
}

func (p *persisted) addSuspect(suspect *pb.Suspect) *actionSet {
	d := &pb.Persistent{
		Type: &pb.Persistent_Suspect{
			Suspect: suspect,
		},
	}

	return p.appendLogEntry(d)
}

func (p *persisted) addECEntry(ecEntry *pb.ECEntry) *actionSet {
	d := &pb.Persistent{
		Type: &pb.Persistent_ECEntry{
			ECEntry: ecEntry,
		},
	}

	return p.appendLogEntry(d)
}

func (p *persisted) addTEntry(tEntry *pb.TEntry) *actionSet {
	d := &pb.Persistent{
		Type: &pb.Persistent_TEntry{
			TEntry: tEntry,
		},
	}

	return p.appendLogEntry(d)
}

func (p *persisted) truncate(lowWatermark uint64) *actionSet {
	for logEntry := p.logHead; logEntry != nil; logEntry = logEntry.next {
		switch d := logEntry.entry.Type.(type) {
		case *pb.Persistent_CEntry:
			if d.CEntry.SeqNo < lowWatermark {
				continue
			}
		case *pb.Persistent_NEntry:
			if d.NEntry.SeqNo <= lowWatermark {
				continue
			}
		default:
			continue
		}

		p.logger.Log(LevelDebug, "truncating WAL", "seq_no", lowWatermark, "index", logEntry.index)

		if p.logHead == logEntry {
			break
		}

		p.logHead = logEntry
		return &actionSet{
			StateEventResult: pb.StateEventResult{
				WriteAhead: []*pb.StateEventResult_Write{
					{
						Truncate: logEntry.index,
					},
				},
			},
		}
	}

	return &actionSet{}
}

// staticcheck hack
var _ = (&persisted{}).logEntries

// logWAL is not called in the course of normal operation but it can be extremely useful
// to call from other parts of the code in debugging situations
func (p *persisted) logEntries() {
	p.logger.Log(LevelDebug, "printing persisted log entries")
	for logEntry := p.logHead; logEntry != nil; logEntry = logEntry.next {
		p.logger.Log(LevelDebug, "  log entry", "type", fmt.Sprintf("%T", logEntry.entry.Type), "index", logEntry.index, "value", fmt.Sprintf("%+v", logEntry.entry))
	}
}

func (p *persisted) iterate(li logIterator) {
	for logEntry := p.logHead; logEntry != nil; logEntry = logEntry.next {
		switch d := logEntry.entry.Type.(type) {
		case *pb.Persistent_PEntry:
			if li.onPEntry != nil {
				li.onPEntry(d.PEntry)
			}
		case *pb.Persistent_QEntry:
			if li.onQEntry != nil {
				li.onQEntry(d.QEntry)
			}
		case *pb.Persistent_CEntry:
			if li.onCEntry != nil {
				li.onCEntry(d.CEntry)
			}
		case *pb.Persistent_NEntry:
			if li.onNEntry != nil {
				li.onNEntry(d.NEntry)
			}
		case *pb.Persistent_FEntry:
			if li.onFEntry != nil {
				li.onFEntry(d.FEntry)
			}
		case *pb.Persistent_ECEntry:
			if li.onECEntry != nil {
				li.onECEntry(d.ECEntry)
			}
		case *pb.Persistent_TEntry:
			if li.onTEntry != nil {
				li.onTEntry(d.TEntry)
			}
		case *pb.Persistent_Suspect:
			if li.onSuspect != nil {
				li.onSuspect(d.Suspect)
			}
			// TODO, suspect_ready
		default:
			panic(fmt.Sprintf("unsupported log entry type '%T'", logEntry.entry.Type))
		}

		if li.shouldExit != nil && li.shouldExit() {
			break
		}
	}
}

func (p *persisted) constructEpochChange(newEpoch uint64) *pb.EpochChange {
	newEpochChange := &pb.EpochChange{
		NewEpoch: newEpoch,
	}

	// To avoid putting redundant entries into the pSet, we count
	// how many are in the log for each sequence so that we may
	// skip all but the last entry for each sequence number
	pSkips := map[uint64]int{}
	var logEpoch *uint64
	p.iterate(logIterator{
		shouldExit: func() bool {
			return logEpoch != nil && *logEpoch >= newEpoch
		},
		onPEntry: func(pEntry *pb.PEntry) {
			count := pSkips[pEntry.SeqNo]
			pSkips[pEntry.SeqNo] = count + 1
		},
		onNEntry: func(nEntry *pb.NEntry) {
			logEpoch = &nEntry.EpochConfig.Number
		},
		onFEntry: func(fEntry *pb.FEntry) {
			logEpoch = &fEntry.EndsEpochConfig.Number
		},
	})

	logEpoch = nil
	p.iterate(logIterator{
		shouldExit: func() bool {
			return logEpoch != nil && *logEpoch >= newEpoch
		},
		onPEntry: func(pEntry *pb.PEntry) {
			count := pSkips[pEntry.SeqNo]
			if count != 1 {
				pSkips[pEntry.SeqNo] = count - 1
				return
			}
			newEpochChange.PSet = append(newEpochChange.PSet, &pb.EpochChange_SetEntry{
				Epoch:  *logEpoch,
				SeqNo:  pEntry.SeqNo,
				Digest: pEntry.Digest,
			})
		},
		onQEntry: func(qEntry *pb.QEntry) {
			newEpochChange.QSet = append(newEpochChange.QSet, &pb.EpochChange_SetEntry{
				Epoch:  *logEpoch,
				SeqNo:  qEntry.SeqNo,
				Digest: qEntry.Digest,
			})
		},
		onNEntry: func(nEntry *pb.NEntry) {
			logEpoch = &nEntry.EpochConfig.Number
		},
		onFEntry: func(fEntry *pb.FEntry) {
			logEpoch = &fEntry.EndsEpochConfig.Number
		},
		onCEntry: func(cEntry *pb.CEntry) {
			newEpochChange.Checkpoints = append(newEpochChange.Checkpoints, &pb.Checkpoint{
				SeqNo: cEntry.SeqNo,
				Value: cEntry.CheckpointValue,
			})
		},
		/*
			// This is actually okay, since we could be catching up and need to skip epochs
					onECEntry: func(ecEntry *pb.ECEntry) {
						if logEpoch != nil && *logEpoch+1 != ecEntry.EpochNumber {
							panic(fmt.Sprintf("dev sanity test: expected epochChange target %d to be exactly one more than our current epoch %d", ecEntry.EpochNumber, *logEpoch))
						}
					},
		*/
	})

	return newEpochChange
}
