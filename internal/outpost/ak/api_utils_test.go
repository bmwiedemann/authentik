package ak

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	api "goauthentik.io/packages/client-go"
)

type fakeAPIType struct{}

type fakeAPIResponse struct {
	results    []fakeAPIType
	pagination api.Pagination
}

func (fapi *fakeAPIResponse) GetResults() []fakeAPIType     { return fapi.results }
func (fapi *fakeAPIResponse) GetPagination() api.Pagination { return fapi.pagination }

type fakeAPIRequest struct {
	res  *fakeAPIResponse
	http *http.Response
	err  error
}

func (fapi *fakeAPIRequest) Page(page int32) *fakeAPIRequest     { return fapi }
func (fapi *fakeAPIRequest) PageSize(size int32) *fakeAPIRequest { return fapi }
func (fapi *fakeAPIRequest) Execute() (*fakeAPIResponse, *http.Response, error) {
	return fapi.res, fapi.http, fapi.err
}

func Test_Simple(t *testing.T) {
	req := &fakeAPIRequest{
		res: &fakeAPIResponse{
			results: []fakeAPIType{
				{},
			},
			pagination: api.Pagination{
				TotalPages: 1,
			},
		},
	}
	res, err := Paginator(req, PaginatorOptions{})
	assert.NoError(t, err)
	assert.Len(t, res, 1)
}

func Test_BadRequest(t *testing.T) {
	req := &fakeAPIRequest{
		http: &http.Response{
			StatusCode: 400,
		},
		err: errors.New("foo"),
	}
	res, err := Paginator(req, PaginatorOptions{})
	assert.Error(t, err)
	assert.Equal(t, []fakeAPIType{}, res)
}

// fakePagedRequest serves a fixed number of pages and can fail a given page
// a given number of times.
type fakePagedRequest struct {
	pages      int32
	failPage   int32
	failTimes  int
	page       int32
	executions int
}

func (f *fakePagedRequest) Page(page int32) *fakePagedRequest     { f.page = page; return f }
func (f *fakePagedRequest) PageSize(size int32) *fakePagedRequest { return f }
func (f *fakePagedRequest) Execute() (*fakeAPIResponse, *http.Response, error) {
	f.executions++
	if f.page == f.failPage && f.failTimes > 0 {
		f.failTimes--
		return nil, &http.Response{StatusCode: 502}, errors.New("bad gateway")
	}
	next := f.page + 1
	if next > f.pages {
		next = 0
	}
	return &fakeAPIResponse{
		results:    []fakeAPIType{{}},
		pagination: api.Pagination{Next: float32(next), TotalPages: float32(f.pages)},
	}, &http.Response{StatusCode: 200}, nil
}

func Test_ServerErrorRetried(t *testing.T) {
	req := &fakePagedRequest{pages: 3, failPage: 2, failTimes: 2}
	res, err := Paginator(req, PaginatorOptions{MaxRetries: 3, RetryBackoff: time.Microsecond})
	assert.NoError(t, err)
	assert.Len(t, res, 3)
	assert.Equal(t, 5, req.executions)
}

func Test_ServerErrorGivesUp(t *testing.T) {
	// Previously an error on any page after the first retried that page
	// forever, so this test would hang.
	req := &fakePagedRequest{pages: 3, failPage: 2, failTimes: 100}
	res, err := Paginator(req, PaginatorOptions{MaxRetries: 2, RetryBackoff: time.Microsecond})
	assert.Error(t, err)
	assert.ErrorContains(t, err, "page 2")
	assert.Len(t, res, 1)
	assert.Equal(t, 1+3, req.executions)
}

// func Test_PaginatorCompile(t *testing.T) {
// 	req := api.ApiCoreUsersListRequest{}
// 	Paginator(req, PaginatorOptions{
// 		PageSize: 100,
// 	})
// }

// func Test_PaginatorCompileExplicit(t *testing.T) {
// 	req := api.ApiCoreUsersListRequest{}
// 	Paginator[
// 		api.User,
// 		api.ApiCoreUsersListRequest,
// 		*api.PaginatedUserList,
// 	](req, PaginatorOptions{
// 		PageSize: 100,
// 	})
// }

// func Test_PaginatorCompileOther(t *testing.T) {
// 	req := api.ApiOutpostsProxyListRequest{}
// 	Paginator(req, PaginatorOptions{
// 		PageSize: 100,
// 	})
// }
