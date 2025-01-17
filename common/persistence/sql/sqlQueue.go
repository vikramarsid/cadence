// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package sql

import (
	"fmt"

	"database/sql"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/persistence/sql/storage/sqldb"
)

type (
	sqlQueue struct {
		queueType common.QueueType
		logger    log.Logger
		sqlStore
	}
)

func newQueue(
	db sqldb.Interface,
	logger log.Logger,
	queueType common.QueueType,
) (persistence.Queue, error) {
	return &sqlQueue{
		sqlStore: sqlStore{
			db:     db,
			logger: logger,
		},
		queueType: queueType,
		logger:    logger,
	}, nil
}

func (q *sqlQueue) EnqueueMessage(messagePayload []byte) error {
	err := q.txExecute("EnqueueMessage", func(tx sqldb.Tx) error {
		lastMessageID, err := tx.GetLastEnqueuedMessageIDForUpdate(q.queueType)
		if err != nil {
			if err == sql.ErrNoRows {
				lastMessageID = -1
			} else {
				return fmt.Errorf("failed to get last enqueued message id: %v", err)
			}
		}

		_, err = tx.InsertIntoQueue(newQueueRow(q.queueType, lastMessageID+1, messagePayload))
		return err
	})
	if err != nil {
		return &workflow.InternalServiceError{Message: err.Error()}
	}
	return nil
}

func (q *sqlQueue) ReadMessages(lastMessageID, maxCount int) ([]*persistence.QueueMessage, error) {
	rows, err := q.db.GetMessagesFromQueue(q.queueType, lastMessageID, maxCount)
	if err != nil {
		return nil, err
	}

	var messages []*persistence.QueueMessage
	for _, row := range rows {
		messages = append(messages, &persistence.QueueMessage{ID: row.MessageID, Payload: row.MessagePayload})
	}
	return messages, nil
}

func newQueueRow(queueType common.QueueType, messageID int, payload []byte) *sqldb.QueueRow {
	return &sqldb.QueueRow{QueueType: queueType, MessageID: messageID, MessagePayload: payload}
}

func (q *sqlQueue) DeleteMessagesBefore(messageID int) error {
	_, err := q.db.DeleteMessagesBefore(q.queueType, messageID)
	if err != nil {
		return &workflow.InternalServiceError{
			Message: fmt.Sprintf("DeleteMessagesBefore operation failed. Error %v", err),
		}
	}
	return nil
}

func (q *sqlQueue) UpdateAckLevel(messageID int, clusterName string) error {
	err := q.txExecute("UpdateAckLevel", func(tx sqldb.Tx) error {
		clusterAckLevels, err := tx.GetAckLevels(q.queueType, true)
		if err != nil {
			return &workflow.InternalServiceError{
				Message: fmt.Sprintf("UpdateAckLevel operation failed. Error %v", err),
			}
		}

		if clusterAckLevels == nil {
			err := tx.InsertAckLevel(q.queueType, messageID, clusterName)
			if err != nil {
				return &workflow.InternalServiceError{
					Message: fmt.Sprintf("UpdateAckLevel operation failed. Error %v", err),
				}
			}
			return nil
		}

		// Ignore possibly delayed message
		if clusterAckLevels[clusterName] > messageID {
			return nil
		}

		clusterAckLevels[clusterName] = messageID
		err = tx.UpdateAckLevels(q.queueType, clusterAckLevels)
		if err != nil {
			return &workflow.InternalServiceError{
				Message: fmt.Sprintf("UpdateAckLevel operation failed. Error %v", err),
			}
		}
		return nil
	})

	if err != nil {
		return &workflow.InternalServiceError{Message: err.Error()}
	}
	return nil
}

func (q *sqlQueue) GetAckLevels() (map[string]int, error) {
	return q.db.GetAckLevels(q.queueType, false)
}
