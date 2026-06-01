package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/oapi"
)

func toDomainForkMailboxes(mailboxes *[]oapi.ForkMailboxPayload) ([]instances.ForkMailboxPayload, error) {
	if mailboxes == nil || len(*mailboxes) == 0 {
		return nil, nil
	}

	out := make([]instances.ForkMailboxPayload, 0, len(*mailboxes))
	for _, mailbox := range *mailboxes {
		payload, err := json.Marshal(mailbox.Payload)
		if err != nil {
			return nil, fmt.Errorf("marshal mailbox %q payload: %w", mailbox.Name, err)
		}
		waitForAck := mailbox.WaitForAck != nil && *mailbox.WaitForAck
		var ackTimeout time.Duration
		if mailbox.AckTimeoutMs != nil {
			ackTimeout = time.Duration(*mailbox.AckTimeoutMs) * time.Millisecond
		}
		out = append(out, instances.ForkMailboxPayload{
			Name:       mailbox.Name,
			Token:      mailbox.Token,
			Payload:    payload,
			WaitForAck: waitForAck,
			AckTimeout: ackTimeout,
		})
	}
	return out, nil
}
