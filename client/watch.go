package client

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/noamsto/forge-cache/forge"
)

// Watch streams the current state of the matching changes, then every update, until ctx is
// cancelled or the daemon closes the stream. number 0 watches the whole repo. The daemon
// keeps the watched keys leased while the stream is open.
func (c *Client) Watch(ctx context.Context, repo forge.RepoRef, number int) (<-chan forge.Event, error) {
	q := url.Values{"repo": {repo.String()}}
	if number != 0 {
		q.Set("number", strconv.Itoa(number))
	}
	resp, err := c.send(ctx, http.MethodGet, "/v1/watch", q, nil)
	if err != nil {
		return nil, err
	}
	ch := make(chan forge.Event)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var ev forge.Event
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				return
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
