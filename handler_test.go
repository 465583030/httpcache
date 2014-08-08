package httpcache

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var testTime time.Time = time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)
var dumpRequests = false

func init() {
	if os.Getenv("DUMP_REQUESTS") != "" {
		dumpRequests = true
	}
}

func TestUpstreamHeadersCopied(t *testing.T) {
	requests := []testRequest{
		testRequest{
			UpstreamHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Copied-Header", "Llamas")
				defaultHandler().ServeHTTP(w, r)
			}),
			Request: NewRequest("GET", "http://example.org/test", nil),
			AssertResponse: func(r *httptest.ResponseRecorder) {
				if r.HeaderMap.Get("X-Copied-Header") == "" {
					t.Fatal("Headers not copied from upstream response")
				}
			},
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestCacheMiss(t *testing.T) {
	requests := []testRequest{
		testRequest{
			Request:           NewRequest("GET", "http://example.org/test", nil),
			AssertCacheStatus: "MISS",
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestCacheHit(t *testing.T) {
	requests := []testRequest{
		testRequest{
			Request:           NewRequest("GET", "http://example.org/test", nil),
			AssertCacheStatus: "MISS",
		},
		testRequest{
			Request:           NewRequest("GET", "http://example.org/test", nil),
			AssertCacheStatus: "HIT",
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestCacheHitWithHead(t *testing.T) {
	requests := []testRequest{
		testRequest{
			Request:           NewRequest("GET", "http://example.org/test", nil),
			AssertCacheStatus: "MISS",
		},
		testRequest{
			Request:             NewRequest("HEAD", "http://example.org/test", nil),
			AssertCacheStatus:   "HIT",
			AssertContentLength: defaultHandler().SizeString(),
			AssertResponse: func(r *httptest.ResponseRecorder) {
				if r.Body.String() != "" {
					t.Fatal("HEAD responses should have no body")
				}
			},
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestCacheAge(t *testing.T) {
	requests := []testRequest{
		testRequest{
			Request: NewRequest("GET", "http://example.org/test", nil),
			Time:    testTime,
			UpstreamHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "max-age=172800")
				defaultHandler().ServeHTTP(w, r)
			}),
		},
		testRequest{
			Request: NewRequest("GET", "http://example.org/test", nil),
			Time:    testTime.AddDate(0, 0, 1),
			AssertResponse: func(r *httptest.ResponseRecorder) {
				assert.Equal(t, "86400", r.HeaderMap.Get("Age"))
			},
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestCacheControlUpstreamNoStore(t *testing.T) {
	requests := []testRequest{
		testRequest{
			Request: NewRequest("GET", "http://example.org/test", nil),
			UpstreamHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store, no-cache")
				defaultHandler().ServeHTTP(w, r)
			}),
			AssertCacheStatus: "SKIP",
		},
		testRequest{
			Request:           NewRequest("GET", "http://example.org/test", nil),
			AssertCacheStatus: "MISS",
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestCacheControlRequest(t *testing.T) {
	table := []struct {
		cacheControl string
		cacheStatus  string
	}{
		{"no-cache", "SKIP"},
	}

	for _, expect := range table {
		requests := []testRequest{
			testRequest{
				Request: NewRequest("GET", "http://example.org/test", http.Header{
					"Cache-Control": []string{expect.cacheControl},
				}),
				AssertCacheStatus: expect.cacheStatus,
			},
		}

		runRequests(requests, NewPrivateCache(), assert.New(t))
	}
}

func TestConditionalResponses(t *testing.T) {
	requests := []testRequest{
		testRequest{
			Request: NewRequest("GET", "http://example.org/test", http.Header{}),
			UpstreamHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Last-Modified", testTime.Format(http.TimeFormat))
				w.Header().Set("ETag", "llamas-rock")
				defaultHandler().ServeHTTP(w, r)
			}),
		},
		testRequest{
			Request: NewRequest("GET", "http://example.org/test", http.Header{
				"If-Modified-Since": []string{testTime.Format(http.TimeFormat)},
			}),
			AssertCode: http.StatusNotModified,
			AssertResponse: func(r *httptest.ResponseRecorder) {
				if r.Body.String() != "" {
					t.Fatal("Response with 304 Not Modified should have no body")
				}
				if expect, got := r.HeaderMap.Get("Etag"), "llamas-rock"; got != expect {
					t.Fatalf("Expected etag %q, got %q", expect, got)
				}
			},
		},
		testRequest{
			Request: NewRequest("GET", "http://example.org/test", http.Header{
				"If-None-Match": []string{"llamas-rock"},
			}),
			AssertCode: http.StatusNotModified,
			AssertResponse: func(r *httptest.ResponseRecorder) {
				if r.Body.String() != "" {
					t.Fatal("Response with 304 Not Modified should have no body")
				}
			},
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestHopByHopHeadersNotSentUpstream(t *testing.T) {
	requests := []testRequest{
		testRequest{
			UpstreamHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defaultHandler().ServeHTTP(w, r)
			}),
			Request: NewRequest("GET", "http://example.org/test", http.Header{
				"Connection": []string{"close"},
			}),
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestCachingConditionalResponses(t *testing.T) {
	requests := []testRequest{
		testRequest{
			UpstreamHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-None-Match") != "llamas-rock" {
					t.Fatal("If-None-Match headers not forwarded upstream")
				}
				w.WriteHeader(http.StatusNotModified)
			}),
			Request: NewRequest("GET", "http://example.org/test", http.Header{
				"If-None-Match": []string{"llamas-rock"},
			}),
			AssertCacheStatus: "MISS",
			AssertCode:        http.StatusNotModified,
		},
		testRequest{
			UpstreamHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("Shouldn't hit upstream for cached requests")
			}),
			Request: NewRequest("GET", "http://example.org/test", http.Header{
				"If-None-Match": []string{"llamas-rock"},
			}),
			AssertCacheStatus: "HIT",
		},
		testRequest{
			Request:           NewRequest("GET", "http://example.org/test", nil),
			AssertCacheStatus: "MISS",
		},
	}

	runRequests(requests, NewPrivateCache(), assert.New(t))
}

func TestStaleResponses(t *testing.T) {
	var table = []struct {
		clientCacheControl, serverCacheControl string
		hasWarning                             bool
		age                                    time.Duration
		status                                 string
	}{
		{"", "max-age=86400", true, time.Hour * 24, "HIT"},
		{"", "max-age=86400", false, time.Hour * 14, "HIT"},
		{"", "max-age=86400", false, time.Hour * 1, "HIT"},
		{"max-age=30", "max-age=86400", true, time.Hour * 1, "HIT"},
	}

	for _, expect := range table {
		requests := []testRequest{
			testRequest{
				Request: NewRequest("GET", "http://example.org/test", http.Header{
					"Cache-Control": []string{expect.clientCacheControl},
				}),
				Time: testTime,
				UpstreamHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Cache-Control", expect.serverCacheControl)
					defaultHandler().ServeHTTP(w, r)
				}),
			},
			testRequest{
				Request: NewRequest("GET", "http://example.org/test", http.Header{
					"Cache-Control": []string{expect.clientCacheControl},
				}),
				Time: testTime.Add(expect.age),
				AssertResponse: func(r *httptest.ResponseRecorder) {
					w := r.HeaderMap.Get("Warning")
					if !strings.HasPrefix(w, "110 - ") && expect.hasWarning {
						t.Fatalf("Expected a Warning 110 header, got %q", w)
					} else if strings.HasPrefix(w, "110 - ") && !expect.hasWarning {
						t.Fatalf("Expected no Warning header, got %q", w)
					}
					assert.Equal(t, r.HeaderMap.Get("Age"),
						fmt.Sprintf("%.f", expect.age.Seconds()))
				},
				AssertCacheStatus: expect.status,
			},
		}

		runRequests(requests, NewPrivateCache(), assert.New(t))
	}
}

func NewRequest(method string, url string, h http.Header) *http.Request {
	r, err := http.NewRequest(method, url, strings.NewReader(""))
	if err != nil {
		panic(err)
	}

	r.Header = h
	return r
}

type testHandler struct {
	body         string
	responseTime time.Time
}

func (h *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Date", h.responseTime.Format(http.TimeFormat))
	b := []byte(h.body)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

func (h *testHandler) Size() int {
	return len(h.body)
}

func (h *testHandler) SizeString() string {
	return strconv.Itoa(h.Size())
}

func defaultHandler() *testHandler {
	return &testHandler{
		body:         "default handler content",
		responseTime: testTime,
	}
}

func debugHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dumpRequests {
			b, err := httputil.DumpRequest(r, true)
			if err != nil {
				panic(err)
			}

			log.Printf("DEBUG ----> Upstream Request ----> \n%s", b)
		}

		h.ServeHTTP(w, r)
	})
}

type testRequest struct {
	// request / response handling
	Request         *http.Request
	UpstreamHandler http.Handler

	// assertions on the response
	AssertResponse      func(*httptest.ResponseRecorder)
	AssertContent       string
	AssertContentLength string
	AssertCacheStatus   string
	AssertCode          int

	// Time is the time used for age calculations
	Time time.Time
}

func (t *testRequest) applyDefaults() *testRequest {
	if t.AssertCode == 0 {
		t.AssertCode = http.StatusOK
	}
	if t.UpstreamHandler == nil {
		h := defaultHandler()
		if !t.Time.IsZero() {
			h.responseTime = t.Time
		}
		t.UpstreamHandler = debugHandler(h)
	} else {
		t.UpstreamHandler = debugHandler(t.UpstreamHandler)
	}
	return t
}

func (t *testRequest) checkAssertions(r *httptest.ResponseRecorder, assert *assert.Assertions) error {
	assert.Equal(t.AssertCode, r.Code)

	cacheStatus := r.HeaderMap.Get(CacheHeader)
	if t.AssertCacheStatus != "" {
		assert.Equal(t.AssertCacheStatus, cacheStatus)
	}

	contentLength := r.HeaderMap.Get("Content-Length")
	if t.AssertContentLength != "" {
		assert.Equal(t.AssertContentLength, contentLength)
	}

	if t.AssertResponse != nil {
		t.AssertResponse(r)
	}

	return nil
}

func (t *testRequest) run(c *Cache) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler := NewHandler(c, t.UpstreamHandler)

	if !t.Time.IsZero() {
		handler.(*CacheHandler).NowFunc = func() time.Time {
			return t.Time
		}
	}

	if dumpRequests {
		b, err := httputil.DumpRequest(t.Request, true)
		if err != nil {
			panic(err)
		}

		log.Printf("DEBUG Request ----> \n%s", b)
	}

	handler.ServeHTTP(recorder, t.Request)
	WaitForWrites()

	if dumpRequests {
		buf := &bytes.Buffer{}
		buf.WriteString(fmt.Sprintf("HTTP/1.1 %d %s\n", recorder.Code, http.StatusText(recorder.Code)))
		recorder.HeaderMap.Write(buf)
		buf.WriteString("\n")
		buf.Write(recorder.Body.Bytes())

		log.Printf("DEBUG Response <---- \n%s", buf.String())
	}

	return recorder
}

func runRequests(reqs []testRequest, c *Cache, a *assert.Assertions) error {
	for _, req := range reqs {
		if err := req.checkAssertions(req.applyDefaults().run(c), a); err != nil {
			return err
		}
	}
	return nil
}
