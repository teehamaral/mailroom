package hooks

import (
	"context"

	"github.com/gomodule/redigo/redis"
	"github.com/jmoiron/sqlx"
	"github.com/nyaruka/goflow/flows"
	"github.com/nyaruka/goflow/flows/events"
	"github.com/nyaruka/mailroom/models"
	"github.com/sirupsen/logrus"
)

func init() {
	models.RegisterEventHook(events.TypeContactLanguageChanged, handleContactLanguageChanged)
}

// CommitLanguageChangesHook is our hook for language changes
type CommitLanguageChangesHook struct{}

var commitLanguageChangesHook = &CommitLanguageChangesHook{}

// Apply applies our contact language change before our commit
func (h *CommitLanguageChangesHook) Apply(ctx context.Context, tx *sqlx.Tx, rp *redis.Pool, org *models.OrgAssets, sessions map[*models.Session][]interface{}) error {
	// build up our list of pairs of contact id and language name
	updates := make([]interface{}, 0, len(sessions))
	for s, e := range sessions {
		// we only care about the last name change
		event := e[len(e)-1].(*events.ContactLanguageChangedEvent)
		updates = append(updates, &languageUpdate{s.ContactID(), event.Language})
	}

	// do our update
	return models.BulkSQL(ctx, "updating contact language", tx, updateContactLanguageSQL, updates)
}

// handleContactLanguageChanged is called when we process a contact language change
func handleContactLanguageChanged(ctx context.Context, tx *sqlx.Tx, rp *redis.Pool, org *models.OrgAssets, session *models.Session, e flows.Event) error {
	event := e.(*events.ContactLanguageChangedEvent)
	logrus.WithFields(logrus.Fields{
		"contact_uuid": session.ContactUUID(),
		"session_id":   session.ID(),
		"language":     event.Language,
	}).Debug("changing contact language")

	session.AddPreCommitEvent(commitLanguageChangesHook, event)
	return nil
}

// struct used for our bulk update
type languageUpdate struct {
	ContactID models.ContactID `db:"id"`
	Language  string           `db:"language"`
}

const updateContactLanguageSQL = `
	UPDATE 
		contacts_contact c
	SET
		language = r.language,
		modified_on = NOW()
	FROM (
		VALUES(:id, :language)
	) AS
		r(id, language)
	WHERE
		c.id = r.id::int
`
