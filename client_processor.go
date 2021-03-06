/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mirbft

import (
	"bytes"
	"container/list"
	"sync"

	"github.com/pkg/errors"

	pb "github.com/IBM/mirbft/mirbftpb"
)

var ErrClientNotExist error = errors.New("client does not exist")

type RequestStore interface {
	GetAllocation(clientID, reqNo uint64) ([]byte, error)
	PutAllocation(clientID, reqNo uint64, digest []byte) error
	GetRequest(requestAck *pb.RequestAck) ([]byte, error)
	PutRequest(requestAck *pb.RequestAck, data []byte) error
	Sync() error
}

// ClientProcessor is the client half of the processor components.
// It accepts client related actions from the state machine and injects
// new client requests.
type ClientProcessor struct {
	mutex        sync.Mutex
	NodeID       uint64
	RequestStore RequestStore
	Hasher       Hasher
	clients      map[uint64]*Client
	ClientWork   ClientWork
}

type ClientWork struct {
	mutex   sync.Mutex
	readyC  chan struct{}
	results *ClientActionResults
}

// Ready return a channel which reads once there are
// results ready to be read via Results().  Note, this
// method must not be invoked concurrently by different
// go routines.
func (cw *ClientWork) Ready() <-chan struct{} {
	cw.mutex.Lock()
	defer cw.mutex.Unlock()
	if cw.readyC == nil {
		cw.readyC = make(chan struct{})
		if cw.results != nil {
			close(cw.readyC)
		}
	}
	return cw.readyC
}

// Results fetches and clears any outstanding results.  The caller
// must have successfully read from the Ready() channel before calling
// or the behavior is undefined.
func (cw *ClientWork) Results() *ClientActionResults {
	cw.mutex.Lock()
	defer cw.mutex.Unlock()
	cw.readyC = nil
	results := cw.results
	cw.results = nil
	return results
}

func (cw *ClientWork) addPersistedReq(ack *pb.RequestAck) {
	cw.mutex.Lock()
	defer cw.mutex.Unlock()
	if cw.results == nil {
		cw.results = &ClientActionResults{}
		if cw.readyC != nil {
			close(cw.readyC)
		}
	}
	cw.results.PersistedRequests = append(cw.results.PersistedRequests, ack)
}

func (cp *ClientProcessor) Client(clientID uint64) *Client {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()
	if cp.clients == nil {
		cp.clients = map[uint64]*Client{}
	}

	c, ok := cp.clients[clientID]
	if !ok {
		c = newClient(clientID, cp.Hasher, cp.RequestStore, &cp.ClientWork)
		cp.clients[clientID] = c
	}
	return c
}

func (cp *ClientProcessor) Process(ca *ClientActions) (*ClientActionResults, error) {
	results := &ClientActionResults{}

	for _, r := range ca.AllocatedRequests {
		client := cp.Client(r.ClientID)
		digest, err := client.allocate(r.ReqNo)
		if err != nil {
			return nil, err
		}

		if digest != nil {
			results.persisted(&pb.RequestAck{
				ClientId: r.ClientID,
				ReqNo:    r.ReqNo,
				Digest:   digest,
			})
			continue
		}
	}

	if err := cp.RequestStore.Sync(); err != nil {
		return nil, errors.WithMessage(err, "could not sync request store, unsafe to continue")
	}

	// XXX address
	/*
	   for _, r := range actions.ForwardRequests {
	           requestData, err := p.RequestStore.Get(r.RequestAck)
	           if err != nil {
	                   panic(fmt.Sprintf("could not store request, unsafe to continue: %s\n", err))
	           }

	           fr := &pb.Msg{
	                   Type: &pb.Msg_ForwardRequest{
	                           &pb.ForwardRequest{
	                                   RequestAck:  r.RequestAck,
	                                   RequestData: requestData,
	                           },
	                   },
	           }
	           for _, replica := range r.Targets {
	                   if replica == p.Node.Config.ID {
	                           p.Node.Step(context.Background(), replica, fr)
	                   } else {
	                           p.Link.Send(replica, fr)
	                   }
	           }
	   }
	*/

	return results, nil
}

type Client struct {
	mutex        sync.Mutex
	clientWork   *ClientWork
	hasher       Hasher
	clientID     uint64
	requestStore RequestStore
	requests     *list.List
	reqNoMap     map[uint64]*list.Element
	nextReqNo    uint64
}

func newClient(clientID uint64, hasher Hasher, reqStore RequestStore, clientWork *ClientWork) *Client {
	return &Client{
		clientID:     clientID,
		clientWork:   clientWork,
		hasher:       hasher,
		requestStore: reqStore,
		requests:     list.New(),
		reqNoMap:     map[uint64]*list.Element{},
	}
}

type clientRequest struct {
	reqNo                 uint64
	localAllocationDigest []byte
	remoteCorrectDigests  [][]byte
}

func (c *Client) allocate(reqNo uint64) ([]byte, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	el, ok := c.reqNoMap[reqNo]
	if ok {
		clientReq := el.Value.(*clientRequest)
		return clientReq.localAllocationDigest, nil
	}

	cr := &clientRequest{
		reqNo: reqNo,
	}
	el = c.requests.PushBack(cr)
	c.reqNoMap[reqNo] = el

	digest, err := c.requestStore.GetAllocation(c.clientID, reqNo)
	if err != nil {
		return nil, errors.WithMessagef(err, "could not get key for %d.%d", c.clientID, reqNo)
	}

	cr.localAllocationDigest = digest

	return digest, nil
}

func (c *Client) NextReqNo() (uint64, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.requests.Len() == 0 {
		return 0, ErrClientNotExist
	}

	return c.nextReqNo, nil
}

func (c *Client) Propose(reqNo uint64, data []byte) error {
	h := c.hasher.New()
	h.Write(data)
	digest := h.Sum(nil)

	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.requests.Len() == 0 {
		return ErrClientNotExist
	}

	if reqNo < c.nextReqNo {
		return nil
	}

	if reqNo > c.nextReqNo {
		return errors.Errorf("client must submit req_no %d next", c.nextReqNo)
	}

	c.nextReqNo++

	el, ok := c.reqNoMap[reqNo]
	previouslyAllocated := ok
	if !ok {
		// TODO, limit the distance ahead a client can allocate?
		el = c.requests.PushBack(&clientRequest{
			reqNo: reqNo,
		})
		c.reqNoMap[reqNo] = el
	}

	cr := el.Value.(*clientRequest)

	if cr.localAllocationDigest != nil {
		if bytes.Equal(cr.localAllocationDigest, digest) {
			return nil
		}

		return errors.Errorf("cannot store request with digest %x, already stored request with different digest %x", digest, cr.localAllocationDigest)
	}

	if len(cr.remoteCorrectDigests) > 0 {
		found := false
		for _, rd := range cr.remoteCorrectDigests {
			if bytes.Equal(rd, digest) {
				found = true
				break
			}
		}

		if !found {
			return errors.New("other known correct digest exist for reqno")
		}
	}

	ack := &pb.RequestAck{
		ClientId: c.clientID,
		ReqNo:    reqNo,
		Digest:   digest,
	}

	err := c.requestStore.PutRequest(ack, data)
	if err != nil {
		return errors.WithMessage(err, "could not store requests")
	}

	err = c.requestStore.PutAllocation(c.clientID, reqNo, digest)
	if err != nil {
		return err
	}
	cr.localAllocationDigest = digest

	if previouslyAllocated {
		c.clientWork.addPersistedReq(ack)
	}

	return nil
}
