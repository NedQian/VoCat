package server

import (
	"context"
	"strconv"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
)

const smsSyncTestPeer = "+447700900123"

// smsSyncTestBase returns a service-centre timestamp that is fresh relative to
// the retention window the receiver applies to unfinished concatenated SMS.
func smsSyncTestBase() time.Time {
	return time.Now().UTC()
}

// newSMSReceiveTestServer wires a server around a modem that exposes exactly the
// messages it is handed and frees a slot when VoCat asks to delete it, which is
// how a real modem recycles storage.
func newSMSReceiveTestServer(
	t *testing.T,
	stored []device.SMSMessage,
) (*Server, *store.Store, *smsDeletionController, string) {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	const (
		deviceID = "ec20-1"
		imei     = "867394042309830"
	)
	if err := database.UpsertDevice(ctx, store.Device{
		ID: deviceID, Name: "EC20", DeviceType: store.DeviceTypePCIeEC20EC25,
		ModemIMEI: imei, SMSEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	controller := &smsDeletionController{
		fakeDeviceController: fakeDeviceController{entry: device.Device{
			ID: deviceID, Discovered: true,
			Snapshot: &device.Snapshot{DeviceID: deviceID, IMEI: imei, IMSI: "23433"},
		}},
		storedMessages: append([]device.SMSMessage(nil), stored...),
	}
	server := &Server{store: database, logger: regionTestLogger(), devices: controller}
	return server, database, controller, deviceID
}

func smsSyncLongMessage(
	at time.Time, first, second string, reference int,
) []device.SMSMessage {
	return []device.SMSMessage{
		{
			Index: 1, Storage: "SM", StorageStatus: device.SMSStatusReceivedUnread,
			Direction: device.SMSDirectionReceived, From: smsSyncTestPeer, Text: first,
			Encoding: device.SMSEncodingGSM7PDU, ServiceCenterTimestamp: &at,
			Concat: &device.SMSConcatInfo{Reference: reference, Total: 2, Sequence: 1},
			RawPDU: first,
		},
		{
			Index: 2, Storage: "SM", StorageStatus: device.SMSStatusReceivedUnread,
			Direction: device.SMSDirectionReceived, From: smsSyncTestPeer, Text: second,
			Encoding: device.SMSEncodingGSM7PDU, ServiceCenterTimestamp: &at,
			Concat: &device.SMSConcatInfo{Reference: reference, Total: 2, Sequence: 2},
			RawPDU: second,
		},
	}
}

func setModemStorage(controller *smsDeletionController, messages []device.SMSMessage) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.storedMessages = append([]device.SMSMessage(nil), messages...)
}

func modemStorageLen(controller *smsDeletionController) int {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return len(controller.storedMessages)
}

func listStoredSMS(t *testing.T, database *store.Store, deviceID string) []store.SMSMessage {
	t.Helper()
	messages, err := database.ListSMSMessages(context.Background(), store.SMSFilter{DeviceID: deviceID})
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

// Two long SMS from the same peer that reuse both the UDH reference and the
// freed storage slot must stay separate rows. The service-centre time is the
// only thing that distinguishes them.
func TestSyncModemSMSKeepsLongMessagesThatReuseReferenceAndSlot(t *testing.T) {
	ctx := context.Background()
	base := smsSyncTestBase()
	server, database, controller, deviceID := newSMSReceiveTestServer(
		t, smsSyncLongMessage(base, "first-a ", "first-b", 3),
	)
	server.syncModemSMS(ctx, deviceID)
	if remaining := modemStorageLen(controller); remaining != 0 {
		t.Fatalf("completed long SMS left %d modem slots behind", remaining)
	}

	// The modem recycles slot 1 and the carrier recycles UDH reference 3.
	setModemStorage(controller, smsSyncLongMessage(base.Add(2*time.Minute), "second-a ", "second-b", 3))
	server.syncModemSMS(ctx, deviceID)

	messages := listStoredSMS(t, database, deviceID)
	if len(messages) != 2 {
		t.Fatalf("stored rows = %d, want 2: %+v", len(messages), messages)
	}
	if messages[0].Body != "second-a second-b" || messages[1].Body != "first-a first-b" {
		t.Fatalf("stored bodies = %q / %q, want the second message first", messages[0].Body, messages[1].Body)
	}
}

// A long SMS whose segments arrive in different scans must merge into one row.
// The incomplete segments stay on the modem so the first-segment slot still
// identifies the group, and they are released once the message completes.
func TestSyncModemSMSMergesLongMessageAcrossPolls(t *testing.T) {
	ctx := context.Background()
	base := smsSyncTestBase()
	firstSegment := smsSyncLongMessage(base, "part one ", "", 9)[0]
	server, database, controller, deviceID := newSMSReceiveTestServer(
		t, []device.SMSMessage{firstSegment},
	)
	server.syncModemSMS(ctx, deviceID)

	if remaining := modemStorageLen(controller); remaining != 1 {
		t.Fatalf("incomplete long SMS retained %d segments, want 1", remaining)
	}
	if messages := listStoredSMS(t, database, deviceID); len(messages) != 1 {
		t.Fatalf("stored rows after the first segment = %+v", messages)
	}

	setModemStorage(controller, smsSyncLongMessage(base, "part one ", "part two", 9))
	server.syncModemSMS(ctx, deviceID)

	messages := listStoredSMS(t, database, deviceID)
	if len(messages) != 1 || messages[0].Body != "part one part two" {
		t.Fatalf("stored rows after the last segment = %+v", messages)
	}
	if !store.ConcatSMSReadyToNotify(messages[0].MessageID, messages[0].Extra) {
		t.Fatalf("merged row is not complete: %+v", messages[0])
	}
	if remaining := modemStorageLen(controller); remaining != 0 {
		t.Fatalf("completed long SMS left %d modem slots behind", remaining)
	}
}

// Repeated scans of the same incomplete long SMS must not create duplicates.
func TestSyncModemSMSRescanOfIncompleteLongMessageIsIdempotent(t *testing.T) {
	ctx := context.Background()
	base := smsSyncTestBase()
	firstSegment := smsSyncLongMessage(base, "only one ", "", 11)[0]
	server, database, controller, deviceID := newSMSReceiveTestServer(
		t, []device.SMSMessage{firstSegment},
	)
	server.syncModemSMS(ctx, deviceID)
	server.syncModemSMS(ctx, deviceID)

	messages := listStoredSMS(t, database, deviceID)
	if len(messages) != 1 {
		t.Fatalf("stored rows after rescan = %+v", messages)
	}
	if store.ConcatSMSReadyToNotify(messages[0].MessageID, messages[0].Extra) {
		t.Fatalf("single segment must stay incomplete: %+v", messages[0])
	}
	if remaining := modemStorageLen(controller); remaining != 1 {
		t.Fatalf("incomplete segment count = %d, want 1 retained", remaining)
	}
}

// Plain messages are released immediately after they are persisted, and a new
// message that reuses the freed slot stays a separate row.
func TestSyncModemSMSReleasesPlainMessages(t *testing.T) {
	ctx := context.Background()
	base := smsSyncTestBase()
	server, database, controller, deviceID := newSMSReceiveTestServer(t, nil)
	for index := 1; index <= 2; index++ {
		at := base.Add(time.Duration(index) * time.Minute)
		body := "plain " + strconv.Itoa(index)
		setModemStorage(controller, []device.SMSMessage{{
			Index: 1, Storage: "SM", StorageStatus: device.SMSStatusReceivedUnread,
			Direction: device.SMSDirectionReceived, From: smsSyncTestPeer, Text: body,
			Encoding: device.SMSEncodingGSM7PDU, ServiceCenterTimestamp: &at,
			RawPDU: "pdu-" + strconv.Itoa(index),
		}})
		server.syncModemSMS(ctx, deviceID)
		if remaining := modemStorageLen(controller); remaining != 0 {
			t.Fatalf("plain message %d left %d modem slots behind", index, remaining)
		}
	}

	messages := listStoredSMS(t, database, deviceID)
	if len(messages) != 2 {
		t.Fatalf("stored rows = %d, want 2: %+v", len(messages), messages)
	}
	if messages[0].Body != "plain 2" || messages[1].Body != "plain 1" {
		t.Fatalf("stored bodies = %q / %q", messages[0].Body, messages[1].Body)
	}
}

// An unfinished long SMS whose missing segments never arrive is released after
// the retention window so it cannot pin the modem storage forever.
func TestSyncModemSMSReleasesExpiredIncompleteSegment(t *testing.T) {
	ctx := context.Background()
	stale := time.Now().UTC().Add(-2 * modemSMSIncompleteRetention)
	segment := smsSyncLongMessage(stale, "stale ", "", 13)[0]
	server, database, controller, deviceID := newSMSReceiveTestServer(
		t, []device.SMSMessage{segment},
	)
	server.syncModemSMS(ctx, deviceID)

	if remaining := modemStorageLen(controller); remaining != 0 {
		t.Fatalf("expired incomplete segment retained %d modem slots, want 0", remaining)
	}
	if messages := listStoredSMS(t, database, deviceID); len(messages) != 1 {
		t.Fatalf("stored rows = %+v", messages)
	}
}
