package broadcasts

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/jmoiron/sqlx"
	"github.com/nyaruka/gocommon/urns"
	"github.com/nyaruka/mailroom"
	"github.com/nyaruka/mailroom/courier"
	"github.com/nyaruka/mailroom/models"
	"github.com/nyaruka/mailroom/queue"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	startBatchSize = 100
)

func init() {
	mailroom.AddTaskFunction(queue.SendBroadcast, handleSendBroadcast)
	mailroom.AddTaskFunction(queue.SendBroadcastBatch, handleSendBroadcastBatch)
}

// handleSendBroadcast creates all the batches of contacts that need to be sent to
func handleSendBroadcast(ctx context.Context, mr *mailroom.Mailroom, task *queue.Task) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*60)
	defer cancel()

	// decode our task body
	if task.Type != queue.SendBroadcast {
		return errors.Errorf("unknown event type passed to send worker: %s", task.Type)
	}
	broadcast := &models.Broadcast{}
	err := json.Unmarshal(task.Task, broadcast)
	if err != nil {
		return errors.Wrapf(err, "error unmarshalling broadcast: %s", string(task.Task))
	}

	return CreateBroadcastBatches(ctx, mr.DB, mr.RP, broadcast)
}

// CreateBroadcastBatches takes our master broadcast and creates batches of broadcast sends for all the unique contacts
func CreateBroadcastBatches(ctx context.Context, db *sqlx.DB, rp *redis.Pool, bcast *models.Broadcast) error {
	// we are building a set of contact ids, start with the explicit ones
	contactIDs := make(map[models.ContactID]bool)
	for _, id := range bcast.ContactIDs() {
		contactIDs[id] = true
	}

	groupContactIDs, err := models.ContactIDsForGroupIDs(ctx, db, bcast.GroupIDs())
	for _, id := range groupContactIDs {
		contactIDs[id] = true
	}

	org, err := models.GetOrgAssets(ctx, db, bcast.OrgID())
	if err != nil {
		return errors.Wrapf(err, "error getting org assets")
	}

	sa, err := models.GetSessionAssets(org)
	if err != nil {
		return errors.Wrapf(err, "error getting session assets")
	}

	// get the contact ids for our URNs
	urnMap, err := models.ContactIDsFromURNs(ctx, db, org, sa, bcast.URNs())
	if err != nil {
		return errors.Wrapf(err, "error getting contact ids for urns")
	}

	urnContacts := make(map[models.ContactID]urns.URN)
	repeatedContacts := make(map[models.ContactID]urns.URN)

	q := queue.BatchQueue

	// two or fewer contacts? queue to our handler queue for sending
	if len(contactIDs) <= 2 {
		q = queue.HandlerQueue
	}

	// we want to remove contacts that are also present in URN sends, these will be a special case in our last batch
	for u, id := range urnMap {
		if contactIDs[id] {
			repeatedContacts[id] = u
			delete(contactIDs, id)
		}
		urnContacts[id] = u
	}

	rc := rp.Get()
	defer rc.Close()

	contacts := make([]models.ContactID, 0, 100)

	// utility functions for queueing the current set of contacts
	queueBatch := func(isLast bool) {
		// if this is our last batch include those contacts that overlap with our urns
		if isLast {
			for id := range repeatedContacts {
				contacts = append(contacts, id)
			}
		}

		batch := bcast.CreateBatch(contacts)

		// also set our URNs
		if isLast {
			batch.SetIsLast(true)
			batch.SetURNs(urnContacts)
		}

		err = queue.AddTask(rc, q, queue.SendBroadcastBatch, int(bcast.OrgID()), batch, queue.DefaultPriority)
		if err != nil {
			logrus.WithError(err).Error("error while queuing broadcast batch")
		}
		contacts = make([]models.ContactID, 0, 100)
	}

	// build up batches of contacts to start
	for c := range contactIDs {
		if len(contacts) == startBatchSize {
			queueBatch(false)
		}
		contacts = append(contacts, c)
	}

	// queue our last batch
	queueBatch(true)

	return nil
}

// handleSendBroadcastBatch sends our messages
func handleSendBroadcastBatch(ctx context.Context, mr *mailroom.Mailroom, task *queue.Task) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*60)
	defer cancel()

	// decode our task body
	if task.Type != queue.SendBroadcastBatch {
		return errors.Errorf("unknown event type passed to send worker: %s", task.Type)
	}
	broadcast := &models.BroadcastBatch{}
	err := json.Unmarshal(task.Task, broadcast)
	if err != nil {
		return errors.Wrapf(err, "error unmarshalling broadcast: %s", string(task.Task))
	}

	// try to send the batch
	return SendBroadcastBatch(ctx, mr.DB, mr.RP, broadcast)
}

// SendBroadcastBatch sends the passed in broadcast batch
func SendBroadcastBatch(ctx context.Context, db *sqlx.DB, rp *redis.Pool, bcast *models.BroadcastBatch) error {
	// always set our broadcast as sent if it is our last
	defer func() {
		if bcast.IsLast() {
			err := models.MarkBroadcastSent(ctx, db, bcast.BroadcastID())
			if err != nil {
				logrus.WithError(err).Error("error marking broadcast as sent")
			}
		}
	}()

	org, err := models.GetOrgAssets(ctx, db, bcast.OrgID())
	if err != nil {
		return errors.Wrapf(err, "error getting org assets")
	}

	sa, err := models.GetSessionAssets(org)
	if err != nil {
		return errors.Wrapf(err, "error getting session assets")
	}

	// create this batch of messages
	msgs, err := models.CreateBroadcastMessages(ctx, db, rp, org, sa, bcast)
	if err != nil {
		return errors.Wrapf(err, "error creating broadcast messages")
	}

	// and queue them to courier for sending
	rc := rp.Get()
	defer rc.Close()

	err = courier.QueueMessages(rc, msgs)
	if err != nil {
		return errors.Wrapf(err, "error queuing broadcast messages")
	}

	return nil
}
