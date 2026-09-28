package runtimed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	sessionOperationJournalDataKey = "journal.json"
	sessionOperationJournalLabel   = "kruntimes.io/session-operation"
	sessionOperationRunUIDLabel    = "kruntimes.io/run-uid"

	maxStoredSessionOperationEvents = 64
	maxStoredSessionOperationBytes  = 8 << 10
	maxSessionOperationJournalBytes = 768 << 10
	sessionOperationSubscriberSize  = 16
)

// sessionOperationStore retains the small, typed event journal for a Session
// operation. Production stores each journal in a ConfigMap owned by the Run so
// reconnects and runtimed restarts can replay it. A nil client is intentionally
// useful for direct proxy unit tests and keeps the storage behavior testable
// without an API server.
type sessionOperationStore struct {
	client client.Client

	mu          sync.Mutex
	memory      map[string]*storedSessionOperation
	active      map[string]bool
	subscribers map[string]map[chan *pb.SessionOperationEvent]struct{}
}

type storedSessionOperation struct {
	OperationID string                        `json:"operationID"`
	Digest      string                        `json:"digest"`
	CreatedAt   metav1.Time                   `json:"createdAt"`
	UpdatedAt   metav1.Time                   `json:"updatedAt"`
	Terminal    bool                          `json:"terminal"`
	Events      []storedSessionOperationEvent `json:"events"`
}

type storedSessionOperationEvent struct {
	Sequence int64  `json:"sequence"`
	Value    []byte `json:"value"`
}

func newSessionOperationStore(apiClient client.Client) *sessionOperationStore {
	return &sessionOperationStore{
		client:      apiClient,
		memory:      make(map[string]*storedSessionOperation),
		active:      make(map[string]bool),
		subscribers: make(map[string]map[chan *pb.SessionOperationEvent]struct{}),
	}
}

// Begin creates the durable journal atomically or returns its existing record.
// The caller uses created to decide whether to submit work or attach to the
// existing operation. The digest fences accidental reuse of an idempotency key.
func (s *sessionOperationStore) Begin(ctx context.Context, run *v1alpha1.Run, operationID, digest string) (*storedSessionOperation, bool, error) {
	if s == nil {
		return nil, false, status.Error(codes.FailedPrecondition, "session operation store is not configured")
	}
	if run == nil || run.UID == "" || run.Namespace == "" || operationID == "" || digest == "" {
		return nil, false, status.Error(codes.InvalidArgument, "session operation journal identity is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionOperationMemoryKey(run, operationID)
	if s.client == nil {
		if existing := s.memory[key]; existing != nil {
			if existing.Digest != digest {
				return nil, false, status.Error(codes.AlreadyExists, "idempotency key was used for a different session operation")
			}
			return cloneStoredSessionOperation(existing), false, nil
		}
		now := metav1.Now()
		record := &storedSessionOperation{OperationID: operationID, Digest: digest, CreatedAt: now, UpdatedAt: now}
		s.memory[key] = record
		s.active[key] = true
		return cloneStoredSessionOperation(record), true, nil
	}

	journal := &corev1.ConfigMap{}
	journalKey := client.ObjectKey{Namespace: run.Namespace, Name: sessionOperationJournalName(run, operationID)}
	err := s.client.Get(ctx, journalKey, journal)
	if err == nil {
		record, err := decodeSessionOperationJournal(journal)
		if err != nil {
			return nil, false, err
		}
		if record.Digest != digest {
			return nil, false, status.Error(codes.AlreadyExists, "idempotency key was used for a different session operation")
		}
		return record, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, status.Errorf(codes.Unavailable, "read session operation journal: %v", err)
	}
	now := metav1.Now()
	record := &storedSessionOperation{OperationID: operationID, Digest: digest, CreatedAt: now, UpdatedAt: now}
	encoded, err := encodeSessionOperationJournal(record)
	if err != nil {
		return nil, false, err
	}
	journal = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      journalKey.Name,
			Namespace: journalKey.Namespace,
			Labels: map[string]string{
				sessionOperationJournalLabel: "true",
				sessionOperationRunUIDLabel:  string(run.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "Run",
				Name:       run.Name,
				UID:        run.UID,
			}},
		},
		Data: map[string]string{sessionOperationJournalDataKey: encoded},
	}
	if err := s.client.Create(ctx, journal); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return s.beginExistingLocked(ctx, journalKey, digest)
		}
		return nil, false, status.Errorf(codes.Unavailable, "create session operation journal: %v", err)
	}
	s.active[key] = true
	return cloneStoredSessionOperation(record), true, nil
}

func (s *sessionOperationStore) beginExistingLocked(ctx context.Context, key client.ObjectKey, digest string) (*storedSessionOperation, bool, error) {
	journal := &corev1.ConfigMap{}
	if err := s.client.Get(ctx, key, journal); err != nil {
		return nil, false, status.Errorf(codes.Unavailable, "read existing session operation journal: %v", err)
	}
	record, err := decodeSessionOperationJournal(journal)
	if err != nil {
		return nil, false, err
	}
	if record.Digest != digest {
		return nil, false, status.Error(codes.AlreadyExists, "idempotency key was used for a different session operation")
	}
	return record, false, nil
}

// Append persists one event before it is delivered to callers. This ordering is
// what makes a cursor a safe replay boundary after a disconnect.
func (s *sessionOperationStore) Append(ctx context.Context, run *v1alpha1.Run, operationID string, event *pb.SessionOperationEvent) error {
	if event == nil || event.GetSequence() <= 0 {
		return status.Error(codes.InvalidArgument, "session operation event sequence is required")
	}
	encodedEvent, err := proto.Marshal(event)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal session operation event: %v", err)
	}
	if len(encodedEvent) > maxStoredSessionOperationBytes {
		return status.Error(codes.ResourceExhausted, "session operation event exceeds retained event limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionOperationMemoryKey(run, operationID)
	if s.client == nil {
		record := s.memory[key]
		if record == nil {
			return status.Error(codes.NotFound, "session operation journal not found")
		}
		if err := appendStoredSessionOperationEvent(record, event, encodedEvent); err != nil {
			return err
		}
		s.broadcastLocked(key, event, record.Terminal)
		if record.Terminal {
			delete(s.active, key)
		}
		return nil
	}
	journal := &corev1.ConfigMap{}
	journalKey := client.ObjectKey{Namespace: run.Namespace, Name: sessionOperationJournalName(run, operationID)}
	if err := s.client.Get(ctx, journalKey, journal); err != nil {
		if apierrors.IsNotFound(err) {
			return status.Error(codes.NotFound, "session operation journal not found")
		}
		return status.Errorf(codes.Unavailable, "read session operation journal: %v", err)
	}
	record, err := decodeSessionOperationJournal(journal)
	if err != nil {
		return err
	}
	if err := appendStoredSessionOperationEvent(record, event, encodedEvent); err != nil {
		return err
	}
	stored, err := encodeSessionOperationJournal(record)
	if err != nil {
		return err
	}
	journal.Data[sessionOperationJournalDataKey] = stored
	if err := s.client.Update(ctx, journal); err != nil {
		return status.Errorf(codes.Unavailable, "persist session operation event: %v", err)
	}
	s.broadcastLocked(key, event, record.Terminal)
	if record.Terminal {
		delete(s.active, key)
	}
	return nil
}

func (s *sessionOperationStore) Active(run *v1alpha1.Run, operationID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[sessionOperationMemoryKey(run, operationID)]
}

func (s *sessionOperationStore) Record(ctx context.Context, run *v1alpha1.Run, operationID string) (*storedSessionOperation, error) {
	if s == nil {
		return nil, status.Error(codes.FailedPrecondition, "session operation store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(ctx, run, operationID)
}

func appendStoredSessionOperationEvent(record *storedSessionOperation, event *pb.SessionOperationEvent, encoded []byte) error {
	if record == nil {
		return status.Error(codes.NotFound, "session operation journal not found")
	}
	if record.Terminal {
		return status.Error(codes.FailedPrecondition, "session operation is already terminal")
	}
	if len(record.Events) >= maxStoredSessionOperationEvents {
		return status.Error(codes.ResourceExhausted, "session operation retained event limit reached")
	}
	if want := int64(len(record.Events) + 1); event.GetSequence() != want {
		return status.Errorf(codes.Internal, "session operation event sequence %d is not contiguous after %d", event.GetSequence(), want-1)
	}
	record.Events = append(record.Events, storedSessionOperationEvent{Sequence: event.GetSequence(), Value: encoded})
	record.UpdatedAt = metav1.Now()
	record.Terminal = event.GetCompleted() != nil || event.GetFailed() != nil
	return nil
}

// Subscribe returns retained events strictly after afterSequence and a bounded
// live channel. If a slow reader fills that channel it is detached; its cursor
// remains durable and it can resume without rerunning the operation.
func (s *sessionOperationStore) Subscribe(ctx context.Context, run *v1alpha1.Run, operationID string, afterSequence int64) ([]*pb.SessionOperationEvent, <-chan *pb.SessionOperationEvent, func(), error) {
	if afterSequence < 0 {
		return nil, nil, nil, status.Error(codes.InvalidArgument, "session operation cursor cannot be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionOperationMemoryKey(run, operationID)
	record, err := s.readLocked(ctx, run, operationID)
	if err != nil {
		return nil, nil, nil, err
	}
	events, err := storedSessionOperationEventsAfter(record, afterSequence)
	if err != nil {
		return nil, nil, nil, err
	}
	updates := make(chan *pb.SessionOperationEvent, sessionOperationSubscriberSize)
	if record.Terminal {
		close(updates)
		return events, updates, func() {}, nil
	}
	if s.subscribers[key] == nil {
		s.subscribers[key] = make(map[chan *pb.SessionOperationEvent]struct{})
	}
	s.subscribers[key][updates] = struct{}{}
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if subscribers := s.subscribers[key]; subscribers != nil {
			if _, ok := subscribers[updates]; ok {
				delete(subscribers, updates)
				close(updates)
			}
		}
	}
	return events, updates, cancel, nil
}

func (s *sessionOperationStore) readLocked(ctx context.Context, run *v1alpha1.Run, operationID string) (*storedSessionOperation, error) {
	if s == nil || run == nil || run.UID == "" {
		return nil, status.Error(codes.InvalidArgument, "session operation identity is required")
	}
	if s.client == nil {
		record := s.memory[sessionOperationMemoryKey(run, operationID)]
		if record == nil {
			return nil, status.Error(codes.NotFound, "session operation not found")
		}
		return cloneStoredSessionOperation(record), nil
	}
	journal := &corev1.ConfigMap{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: sessionOperationJournalName(run, operationID)}, journal); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Error(codes.NotFound, "session operation not found")
		}
		return nil, status.Errorf(codes.Unavailable, "read session operation journal: %v", err)
	}
	return decodeSessionOperationJournal(journal)
}

func (s *sessionOperationStore) broadcastLocked(key string, event *pb.SessionOperationEvent, terminal bool) {
	for subscriber := range s.subscribers[key] {
		select {
		case subscriber <- proto.Clone(event).(*pb.SessionOperationEvent):
		default:
			delete(s.subscribers[key], subscriber)
			close(subscriber)
		}
	}
	if !terminal {
		return
	}
	for subscriber := range s.subscribers[key] {
		delete(s.subscribers[key], subscriber)
		close(subscriber)
	}
}

func storedSessionOperationEventsAfter(record *storedSessionOperation, afterSequence int64) ([]*pb.SessionOperationEvent, error) {
	if record == nil {
		return nil, status.Error(codes.NotFound, "session operation not found")
	}
	events := make([]*pb.SessionOperationEvent, 0, len(record.Events))
	for _, stored := range record.Events {
		if stored.Sequence <= afterSequence {
			continue
		}
		event := &pb.SessionOperationEvent{}
		if err := proto.Unmarshal(stored.Value, event); err != nil {
			return nil, status.Errorf(codes.Internal, "decode stored session operation event: %v", err)
		}
		events = append(events, event)
	}
	return events, nil
}

func encodeSessionOperationJournal(record *storedSessionOperation) (string, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", status.Errorf(codes.Internal, "encode session operation journal: %v", err)
	}
	if len(encoded) > maxSessionOperationJournalBytes {
		return "", status.Error(codes.ResourceExhausted, "session operation retained journal limit reached")
	}
	return string(encoded), nil
}

func decodeSessionOperationJournal(journal *corev1.ConfigMap) (*storedSessionOperation, error) {
	if journal == nil || journal.Data == nil || journal.Data[sessionOperationJournalDataKey] == "" {
		return nil, status.Error(codes.Internal, "session operation journal is invalid")
	}
	record := &storedSessionOperation{}
	if err := json.Unmarshal([]byte(journal.Data[sessionOperationJournalDataKey]), record); err != nil {
		return nil, status.Errorf(codes.Internal, "decode session operation journal: %v", err)
	}
	if record.OperationID == "" || record.Digest == "" || len(record.Events) > maxStoredSessionOperationEvents {
		return nil, status.Error(codes.Internal, "session operation journal is invalid")
	}
	return record, nil
}

func sessionOperationMemoryKey(run *v1alpha1.Run, operationID string) string {
	if run == nil {
		return operationID
	}
	return string(run.UID) + "/" + operationID
}

func sessionOperationJournalName(run *v1alpha1.Run, operationID string) string {
	sum := sha256.Sum256([]byte(sessionOperationMemoryKey(run, operationID)))
	return "kruntimes-session-operation-" + hex.EncodeToString(sum[:])[:24]
}

func cloneStoredSessionOperation(record *storedSessionOperation) *storedSessionOperation {
	if record == nil {
		return nil
	}
	clone := *record
	clone.Events = make([]storedSessionOperationEvent, len(record.Events))
	for index, event := range record.Events {
		clone.Events[index] = storedSessionOperationEvent{Sequence: event.Sequence, Value: append([]byte(nil), event.Value...)}
	}
	return &clone
}

func sessionOperationRequestDigest(request *pb.ExecuteSessionOperationRequest) (string, error) {
	if request == nil || request.GetOperation() == nil {
		return "", errors.New("session operation payload is required")
	}
	clone := proto.Clone(request).(*pb.ExecuteSessionOperationRequest)
	clone.Identity = nil
	clone.IdempotencyKey = ""
	clone.ResumeAfterSequence = 0
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return "", fmt.Errorf("marshal session operation for idempotency: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func sessionOperationPersistenceContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
