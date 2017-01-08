package httpd

import (
	"time"
	rafty_objects "github.com/TykTechnologies/tyk-cluster-framework/rafty/objects"
	"github.com/foize/go.fifo"

	"sync"
	"encoding/json"
	"github.com/TykTechnologies/logrus"
)

const (
	TTLSNAPSHOT_KEY = "TCF_TTL_SNAPHOT"
)

type SnapshotStatus int

const (
	StatusSnapshotFound SnapshotStatus = 1
	StatusSnapshotNotFound SnapshotStatus = 2
	StatusSnapshotNotLeader SnapshotStatus = 3
)

type qSnapShot struct {
	qmu sync.Mutex
	queueSnapshot map[string]ttlIndexElement
}

func (q *qSnapShot) GetSnapshot() map[string]ttlIndexElement {
	q.qmu.Lock()

	thisCopy := q.queueSnapshot
	q.qmu.Unlock()
	return thisCopy
}

func newQueueSnapShot() *qSnapShot {
	return &qSnapShot{
		queueSnapshot: make(map[string]ttlIndexElement),
	}
}

// StorageAPI exposes the raw store getters and setters and adds a TTL handler
type StorageAPI struct{
	store Store
	ttlIndex *fifo.Queue
	queueSnapshot *qSnapShot
	TTLChunkSize int
}

func NewStorageAPI(store Store) *StorageAPI {
	thisSA :=  &StorageAPI{
		store: store,
		ttlIndex: fifo.NewQueue(),
		queueSnapshot: newQueueSnapShot(),
		TTLChunkSize: 100,
	}

	log.WithFields(logrus.Fields{
		"prefix": "tcf.rafty.storage-api",
	}).Info("Starting TTL Processor")
	go thisSA.processTTLs()

	return thisSA
}

func (s *StorageAPI) GetKey(k string, evenIfExpired bool) (*KeyValueAPIObject, *ErrorResponse) {
	// Get the existing value
	v, errResp := s.getKeyFromStore(k)
	if errResp != nil {
		return nil, errResp
	}

	// Decode it
	returnValue, err := NewKeyValueAPIObjectFromMsgPack(v)
	if err != nil {
		return nil, NewErrorResponse("/"+k, "Key marshalling failed: " + err.Error())

	}

	if time.Now().After(returnValue.Node.Expiration) && returnValue.Node.TTL != 0 && evenIfExpired == false {
		log.WithFields(logrus.Fields{
			"prefix": "tcf.rafty.storage-api",
		}).Debug("KEY EXISTS BUT HAS EXPIRED")
		return nil, NewErrorNotFound("/"+k)
	}

	return returnValue, nil
}

func (s *StorageAPI) SetKey(k string, value *rafty_objects.NodeValue, overwrite bool) (*rafty_objects.NodeValue, *ErrorResponse) {
	// Don't allow overwriting unless expired
	existingValue, errResp := s.getKeyFromStore(k)
	asNode, checkErr := NewKeyValueAPIObjectFromMsgPack(existingValue)

	var allowOverwrite = overwrite
	if checkErr == nil {
		if time.Now().After(asNode.Node.Expiration) && asNode.Node.TTL != 0 {
			allowOverwrite = true
		}
	}

	if errResp == nil && allowOverwrite == false {
		keyExistsErr := &ErrorResponse{Cause: "/"+k, Error: RAFTErrorKeyExists}
		return nil, keyExistsErr
	}

	value.Key = k

	// Set expiry value
	value.CalculateExpiry()
	value.Created = time.Now().Unix()
	toStore, encErr := value.EncodeForStorage()

	if encErr != nil {
		return nil, NewErrorResponse("/"+k, "Could not encode payload for store: "+encErr.Error())
	}

	// Write data to the store
	if err := s.store.Set(k, toStore); err != nil {
		return nil, NewErrorResponse("/"+k, "Could not write to store: "+err.Error())
	}

	// Track the TTL
	if value.TTL > 0  && value.Key != TTLSNAPSHOT_KEY {
		s.trackTTLForKey(value.Key, value.Expiration.Unix())
	}

	return value, nil
}

func (s *StorageAPI) DeleteKey(k string) (*KeyValueAPIObject, *ErrorResponse) {
	if err := s.store.Delete(k); err != nil {
		return nil, NewErrorResponse("/"+k, "Delete failed: " + err.Error())
	}

	return nil, nil
}

func (s *StorageAPI) getKeyFromStore(k string) ([]byte, *ErrorResponse) {
	if k == "" {
		return nil, NewErrorResponse("/"+k, "Key cannot be empty")
	}

	v, err := s.store.Get(k)
	if err != nil {
		thisErr := NewErrorNotFound("/"+k)
		thisErr.MetaData = err.Error()
		return nil, thisErr
	}

	if v == nil {
		thisErr := NewErrorNotFound("/"+k)
		return nil, thisErr
	}

	return v, nil
}

type ttlIndexElement struct {
	TTL int64
	Key string
	Index int
}

func (s *StorageAPI) trackTTLForKey(key string, expires int64) {
	s.addTTL(ttlIndexElement{TTL: expires, Key: key})
}

func (s *StorageAPI) addTTL(elem ttlIndexElement) {
	if s.store.IsLeader() == false {
		return
	}

	s.ttlIndex.Add(elem)

	// Store the change in our snapshot
	elem.Index = s.ttlIndex.Len()
	s.queueSnapshot.qmu.Lock()
	defer s.queueSnapshot.qmu.Unlock()
	s.queueSnapshot.queueSnapshot[elem.Key] = elem
}

func (s *StorageAPI) processTTLElement() {
	if s.store.IsLeader() == false {
		return
	}

	max := s.TTLChunkSize
	if s.ttlIndex.Len() < s.TTLChunkSize {
		max = s.ttlIndex.Len()
	}
	applyDeletes := make([]ttlIndexElement, max)
	for i := 0; i < max; i++ {
		var skip bool
		log.WithFields(logrus.Fields{
			"prefix": "tcf.rafty.storage-api",
		}).Debug("Getting next element")
		thisElem := s.ttlIndex.Next()

		if thisElem == nil {
			log.WithFields(logrus.Fields{
				"prefix": "tcf.rafty.storage-api",
			}).Info("Element is empty - end of TTL queue")

			// End of list, break out, no need to continue
			break
		}

		// It's not in the queue, aso it shouldn't be in the snapshot
		applyDeletes[i] = thisElem.(ttlIndexElement)

		existingKey, getErr := s.GetKey(thisElem.(ttlIndexElement).Key, true)
		if getErr != nil {
			// can't get the key, no need to delete it
			skip = true
		}

		if existingKey.Node.Expiration.Unix() != thisElem.(ttlIndexElement).TTL {
			// Expiration has changed, so it must be in the queue again
			log.WithFields(logrus.Fields{
				"prefix": "tcf.rafty.storage-api",
			}).Info("Skipping eviction for key, TTL has changed")
			skip = true
		}

		if skip == false {
			// Check expiry
			tn := time.Now().Unix()
			log.WithFields(logrus.Fields{
				"prefix": "tcf.rafty.storage-api",
			}).Debug("Exp is: ", thisElem.(ttlIndexElement).TTL)
			log.WithFields(logrus.Fields{
				"prefix": "tcf.rafty.storage-api",
			}).Debug("Now is: ", tn)

			if tn > thisElem.(ttlIndexElement).TTL {
				log.WithFields(logrus.Fields{
					"prefix": "tcf.rafty.storage-api",
				}).Info("-> Removing key because expired")
				s.DeleteKey(thisElem.(ttlIndexElement).Key)
			} else {
				log.WithFields(logrus.Fields{
					"prefix": "tcf.rafty.storage-api",
				}).Info("Expiry not reached yet, adding back to stack")
				s.addTTL(thisElem.(ttlIndexElement))
			}
		}


	}

	// Lets apply the delete operations to our queue
	for _, elem := range(applyDeletes) {
		s.queueSnapshot.qmu.Lock()
		delete(s.queueSnapshot.queueSnapshot, elem.Key)
		log.WithFields(logrus.Fields{
			"prefix": "tcf.rafty.storage-api",
		}).Debug("Updated queue snapshot")
		s.queueSnapshot.qmu.Unlock()
	}

	// Store the bulk snapshot change
	s.storeTTLSnapshot()
}

func (s *StorageAPI) rebuildFromSnapshot(intoFifoQ *fifo.Queue) SnapshotStatus {
	if s.store.IsLeader() == false {
		return StatusSnapshotNotLeader
	}

	existingSnapShot, err := s.GetKey(TTLSNAPSHOT_KEY, false)
	if err != nil {
		return StatusSnapshotNotFound
	}

	if existingSnapShot == nil {
		return StatusSnapshotNotFound
	}

	var snapShotAsArray map[string]ttlIndexElement
	decErr := json.Unmarshal([]byte(existingSnapShot.Node.Value), &snapShotAsArray)
	if decErr != nil {
		log.WithFields(logrus.Fields{
			"prefix": "tcf.rafty.storage-api",
		}).Error("Failed to decode snapshot backup")
		return StatusSnapshotNotFound
	}

	for _, elem := range snapShotAsArray {
		log.WithFields(logrus.Fields{
			"prefix": "tcf.rafty.storage-api",
		}).Debug("ADDING SNAPSHOT: ", elem.Key)
		intoFifoQ.Add(elem)
	}

	return StatusSnapshotFound
}

func (s *StorageAPI) storeTTLSnapshot() {
	if s.store.IsLeader() == false {
		return
	}

	thisSnapShot := s.queueSnapshot.GetSnapshot()
	asStr, encErr := json.Marshal(thisSnapShot)

	if encErr != nil {
		log.WithFields(logrus.Fields{
			"prefix": "tcf.rafty.storage-api",
		}).Error("Failed to decode TTL Snapshot")
		return
	}

	existingSnapShot, err := s.GetKey(TTLSNAPSHOT_KEY, false)
	if err != nil {
		existingSnapShot = NewKeyValueAPIObject()
		existingSnapShot.Node.Key = TTLSNAPSHOT_KEY
	}

	existingSnapShot.Node.Value = string(asStr)
	s.SetKey(TTLSNAPSHOT_KEY, existingSnapShot.Node, true)
}

func (s *StorageAPI) processTTLs() {
	for {
		if s.store.IsLeader() {
			// No queue? try to rebuild or reset it
			if s.queueSnapshot == nil {
				log.Debug("Queue is empty, attempting to rebuild")
				s.queueSnapshot = newQueueSnapShot()
				status := s.rebuildFromSnapshot(s.ttlIndex)
				if status == StatusSnapshotFound {
					log.Debug("No snapshot found, initialising a fresh queue")
					s.storeTTLSnapshot()
				}
			}

			// Process the next TTL item (but store snapshot in case we fail)
			s.processTTLElement()
		} else {
			// Not a leader, kill the queue we have
			s.queueSnapshot = nil
		}

		// Process every few seconds
		time.Sleep(5 * time.Second)
	}
}