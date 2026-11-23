package client

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/noamsto/forge-cache/forge"
)

// SubscribeOptions selects the events to receive. Number 0 watches the whole repo, empty
// Types every type, and a non-nil Since replays buffered events after that seq first.
type SubscribeOptions struct {
	Repo   forge.RepoRef
	Number int
	Types  []forge.EventType
	Since  *uint64
}

// Subscribe streams the typed change events matching o until ctx is cancelled or the
// daemon closes the stream. The daemon keeps the watched keys leased while the stream is open.
func (c *Client) Subscribe(ctx context.Context, o SubscribeOptions) (<-chan forge.ChangeEvent, error) {
	q := url.Values{"repo": {o.Repo.String()}}
	if o.Number != 0 {
		q.Set("number", strconv.Itoa(o.Number))
	}
	if len(o.Types) > 0 {
		names := make([]string, len(o.Types))
		for i, t := range o.Types {
			names[i] = string(t)
		}
		q.Set("types", strings.Join(names, ","))
	}
	if o.Since != nil {
		q.Set("since", strconv.FormatUint(*o.Since, 10))
	}
	body, err := c.send(ctx, http.MethodGet, "/v1/watch", q, nil)
	if err != nil {
		return nil, err
	}
	ch := make(chan forge.ChangeEvent)
	go stream(ctx, body, ch)
	return ch, nil
}

// stream decodes newline-delimited events from body into ch, closing both when it ends.
func stream(ctx context.Context, body io.ReadCloser, ch chan<- forge.ChangeEvent) {
	defer close(ch)
	defer func() { _ = body.Close() }()
	sc := bufio.NewScanner(body)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var ev forge.ChangeEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			return
		}
		select {
		case ch <- ev:
		case <-ctx.Done():
			return
		}
	}
}
