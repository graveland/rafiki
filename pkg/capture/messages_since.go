// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"time"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/store"
)

// MessagesSince returns the conversation's rows created at or after since:
// the rows one capture turn wrote, read back rather than collected from
// insert results, so a retried or lost-acknowledgement insert still counts.
func (s *CaptureStore) MessagesSince(ctx context.Context, convID string, since time.Time) ([]*conv.Node, error) {
	var out []*conv.Node
	err := retryDB(ctx, "messagesSince", func(ctx context.Context) error {
		var lerr error
		out, lerr = store.NewMessages(s.pool).LoadSince(ctx, convID, since)
		return lerr
	})
	return out, err
}
