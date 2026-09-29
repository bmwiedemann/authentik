package ak

import (
	"fmt"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
	api "goauthentik.io/packages/client-go"
)

// Generic interface that mimics a generated request by the API client
// Requires mainly `Treq` which will be the actual request type, and
// `Tres` which is the response type
type PaginatorRequest[Treq any, Tres any] interface {
	Page(page int32) Treq
	PageSize(size int32) Treq
	Execute() (Tres, *http.Response, error)
}

// Generic interface that mimics a generated response by the API client
type PaginatorResponse[Tobj any] interface {
	GetResults() []Tobj
	GetPagination() api.Pagination
}

// Paginator options for page size
type PaginatorOptions struct {
	PageSize int
	Logger   *log.Entry
	// MaxRetries is how often a page that failed with a server or network
	// error is fetched again before the whole request is given up. Client
	// errors (4xx) are never retried. Defaults to DefaultPaginatorRetries.
	MaxRetries int
	// RetryBackoff is the wait before the first retry, doubled on every
	// further one. Defaults to DefaultPaginatorBackoff.
	RetryBackoff time.Duration
}

const (
	DefaultPaginatorRetries = 3
	DefaultPaginatorBackoff = 500 * time.Millisecond
)

// Automatically fetch all objects from an API endpoint using the pagination
// data received from the server.
//
// The result is only complete when the returned error is nil. On an error the
// objects fetched so far are returned with it, so callers must not treat a
// partial list as the full directory.
func Paginator[Tobj any, Treq any, Tres PaginatorResponse[Tobj]](
	req PaginatorRequest[Treq, Tres],
	opts PaginatorOptions,
) ([]Tobj, error) {
	if opts.Logger == nil {
		opts.Logger = log.NewEntry(log.StandardLogger())
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = DefaultPaginatorRetries
	}
	if opts.RetryBackoff == 0 {
		opts.RetryBackoff = DefaultPaginatorBackoff
	}
	var bfreq, cfreq any
	// fetchOffset fetches one page, retrying on server or network errors.
	fetchOffset := func(page int32) (Tres, error) {
		var res Tres
		var err error
		backoff := opts.RetryBackoff
		for attempt := 0; ; attempt++ {
			bfreq = req.Page(page)
			cfreq = bfreq.(PaginatorRequest[Treq, Tres]).PageSize(int32(opts.PageSize))
			var hres *http.Response
			res, hres, err = cfreq.(PaginatorRequest[Treq, Tres]).Execute()
			if err == nil {
				return res, nil
			}
			opts.Logger.WithError(err).WithField("page", page).WithField("attempt", attempt).Warning("failed to fetch page")
			if hres != nil && hres.StatusCode >= 400 && hres.StatusCode < 500 {
				return res, err
			}
			if attempt >= opts.MaxRetries {
				return res, err
			}
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	var page int32 = 1
	objects := make([]Tobj, 0)
	for {
		apiObjects, err := fetchOffset(page)
		if err != nil {
			return objects, fmt.Errorf("failed to fetch page %d: %w", page, err)
		}
		objects = append(objects, apiObjects.GetResults()...)
		if apiObjects.GetPagination().Next > 0 {
			page += 1
		} else {
			break
		}
	}
	return objects, nil
}
