package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// Bulk queues many notifications with one round trip per chunk of MaxBulkMessages
// messages. Chunks are sent one at a time, in order, and a chunk that fails does not stop
// the ones after it — check the returned BulkResult for partial success.
//
// The service fans the messages out asynchronously, so a successful chunk means the
// messages were accepted, not delivered. Each accepted chunk is reported as a BulkBatch
// whose BatchID groups those messages in the service.
//
// Every message is validated before the first request is sent, so an invalid message
// anywhere in the slice cannot leave half the batch dispatched.
func (c *Client) Bulk(ctx context.Context, p BulkParams) (*BulkResult, error) {
	chunks, err := prepareBulk(p, 0)
	if err != nil {
		return nil, err
	}

	result := &BulkResult{Batches: make([]BulkBatch, 0, len(chunks))}
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			result.Failures = append(result.Failures, BulkFailure{
				Offset: chunk.offset,
				Size:   chunk.size(),
				Err:    err,
			})
			continue
		}
		batch, err := c.sendBulkChunk(ctx, p.Channel, p.Messages[chunk.offset:chunk.end])
		if err != nil {
			result.Failures = append(result.Failures, BulkFailure{
				Offset: chunk.offset,
				Size:   chunk.size(),
				Err:    err,
			})
			continue
		}
		batch.Offset = chunk.offset
		result.Batches = append(result.Batches, *batch)
		result.Total += batch.Count
	}
	return result, result.Err()
}

// BulkConcurrent is Bulk with several chunk requests in flight at once, which is the
// right choice when the batch is much larger than MaxBulkMessages and the service's rate
// limit allows it.
//
// Concurrency is bounded by BulkOptions.Concurrency (default DefaultBulkConcurrency) and
// a fixed worker pool does the sending, so memory stays flat however large the batch is.
// A failing chunk does not cancel the others: the caller gets every failure rather than
// only the first. Cancelling ctx stops both pending and in-flight chunks, and the chunks
// that never ran are reported as failures carrying the context error.
//
// Results — batches and failures — are always reported in input order regardless of
// completion order, but the requests themselves are dispatched as workers free up, so the
// service may receive chunks out of order. Use Bulk when the dispatch order matters.
func (c *Client) BulkConcurrent(ctx context.Context, p BulkParams, opts BulkOptions) (*BulkResult, error) {
	chunks, err := prepareBulk(p, opts.ChunkSize)
	if err != nil {
		return nil, err
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultBulkConcurrency
	}
	if concurrency > len(chunks) {
		concurrency = len(chunks)
	}

	// One slot per chunk, written by exactly one worker, so aggregation needs no lock and
	// results stay in input order.
	type slot struct {
		batch BulkBatch
		ok    bool
		err   error
	}
	slots := make([]slot, len(chunks))

	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := ctx.Err(); err != nil {
					slots[i].err = err
					continue
				}
				chunk := chunks[i]
				batch, err := c.sendBulkChunk(ctx, p.Channel, p.Messages[chunk.offset:chunk.end])
				if err != nil {
					slots[i].err = err
					continue
				}
				batch.Offset = chunk.offset
				slots[i].batch = *batch
				slots[i].ok = true
			}
		}()
	}

	// Feed the pool in input order. On cancellation the chunks that never made it into
	// the queue are accounted for here, so every input message is always reported either
	// as accepted or as failed.
feed:
	for i := range chunks {
		select {
		case jobs <- i:
		case <-ctx.Done():
			for j := i; j < len(chunks); j++ {
				slots[j].err = ctx.Err()
			}
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	result := &BulkResult{Batches: make([]BulkBatch, 0, len(chunks))}
	for i := range slots {
		if slots[i].ok {
			result.Batches = append(result.Batches, slots[i].batch)
			result.Total += slots[i].batch.Count
			continue
		}
		result.Failures = append(result.Failures, BulkFailure{
			Offset: chunks[i].offset,
			Size:   chunks[i].size(),
			Err:    slots[i].err,
		})
	}
	return result, result.Err()
}

// bulkChunk is a half-open range of the caller's message slice.
type bulkChunk struct {
	offset int
	end    int
}

// size is the number of messages in the chunk.
func (c bulkChunk) size() int { return c.end - c.offset }

// prepareBulk validates the whole batch and splits it into request-sized chunks.
func prepareBulk(p BulkParams, chunkSize int) ([]bulkChunk, error) {
	if err := validateBulk(p); err != nil {
		return nil, err
	}
	if chunkSize <= 0 || chunkSize > MaxBulkMessages {
		chunkSize = MaxBulkMessages
	}

	chunks := make([]bulkChunk, 0, (len(p.Messages)+chunkSize-1)/chunkSize)
	for offset := 0; offset < len(p.Messages); offset += chunkSize {
		end := min(offset+chunkSize, len(p.Messages))
		chunks = append(chunks, bulkChunk{offset: offset, end: end})
	}
	return chunks, nil
}

// validateBulk applies the service's rules to every message up front.
func validateBulk(p BulkParams) error {
	if p.Channel == "" {
		return invalidArgument("channel is required")
	}
	if len(p.Messages) == 0 {
		return invalidArgument("at least one message is required")
	}
	for i := range p.Messages {
		m := &p.Messages[i]
		if strings.TrimSpace(m.Recipient) == "" {
			return invalidArgument("messages[%d]: recipient is required", i)
		}
		if m.Body == "" && m.TemplateID == "" && m.TemplateName == "" {
			return invalidArgument("messages[%d]: one of body, template_id or template_name is required", i)
		}
	}
	return nil
}

// sendBulkChunk posts one chunk and returns the service's acknowledgement.
func (c *Client) sendBulkChunk(ctx context.Context, channel ChannelType, messages []BulkMessage) (*BulkBatch, error) {
	wire := make([]bulkMessage, len(messages))
	for i := range messages {
		wire[i] = messages[i].wire()
	}

	var out bulkResponse
	req := bulkRequest{ChannelType: channel, Messages: wire}
	if err := c.doJSON(ctx, http.MethodPost, pathBulkNotifications, nil, req, &out); err != nil {
		return nil, err
	}
	return &BulkBatch{BatchID: out.BatchID, Count: out.Count}, nil
}

// bulkRequest is the wire form of POST /v1/notifications/bulk.
type bulkRequest struct {
	ChannelType ChannelType   `json:"channel_type"`
	Messages    []bulkMessage `json:"messages"`
}

// bulkMessage is the wire form of one entry in a bulk request.
type bulkMessage struct {
	Recipient      string          `json:"recipient"`
	Subject        string          `json:"subject,omitempty"`
	Body           string          `json:"body,omitempty"`
	TemplateID     string          `json:"template_id,omitempty"`
	TemplateName   string          `json:"template_name,omitempty"`
	TemplateVars   map[string]any  `json:"template_vars,omitempty"`
	TemplateParams []string        `json:"template_params,omitempty"`
	GroupID        string          `json:"group_id,omitempty"`
	GroupName      string          `json:"group_name,omitempty"`
	CallbackURL    string          `json:"callback_url,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

// wire renders one bulk message, applying the "group:template" shorthand.
func (m BulkMessage) wire() bulkMessage {
	group := m.Group
	templateName := m.TemplateName
	if templateName != "" {
		if prefix, name := splitGroupTemplate(templateName); prefix != "" {
			if group == "" {
				group = prefix
			}
			templateName = name
		}
	}
	return bulkMessage{
		Recipient:      m.Recipient,
		Subject:        m.Subject,
		Body:           m.Body,
		TemplateID:     m.TemplateID,
		TemplateName:   templateName,
		TemplateVars:   m.Vars,
		TemplateParams: m.Values,
		GroupID:        m.GroupID,
		GroupName:      group,
		CallbackURL:    m.CallbackURL,
		Metadata:       m.Metadata,
	}
}

// bulkResponse is the service's reply to a bulk request.
type bulkResponse struct {
	BatchID string `json:"batch_id"`
	Count   int    `json:"count"`
}
