package sdk

// The mirror of api's model routes (CSRV-M11-01; DESIGN-COMPUTE §14.3, CO-49): each fetches its object's bytes from
// the route's path, with no credential, cookie or client info though the Api holds a sidecar's token; a request the
// route would refuse is not sent; an answer past its bound is refused; and a refusal is a status error, as connect's
// raw functions answer one.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// One request a test api server saw.
type computeModelSeenRequest struct {
	method        string
	path          string
	rawQuery      string
	authorization string
	cookie        string
	clientInfo    string
}

// An api server answering the model routes of sha256Text from objects by path, recording each request.
type computeModelTestServer struct {
	stateLock sync.Mutex
	requests  []computeModelSeenRequest
	answer    func(w http.ResponseWriter, r *http.Request)
}

func (self *computeModelTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	self.requests = append(self.requests, computeModelSeenRequest{
		method:        r.Method,
		path:          r.URL.Path,
		rawQuery:      r.URL.RawQuery,
		authorization: r.Header.Get("Authorization"),
		cookie:        r.Header.Get("Cookie"),
		clientInfo:    r.Header.Get(connect.ClientInfoHeader),
	})
	answer := self.answer
	self.stateLock.Unlock()
	answer(w, r)
}

func (self *computeModelTestServer) seen() []computeModelSeenRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]computeModelSeenRequest{}, self.requests...)
}

// The index and chunk 2 of a file, through each form, from the routes' paths and with nothing that names the
// caller, though the Api holds a sidecar's token.
func TestComputeModelApiFetchesWithNoCredential(t *testing.T) {
	sha256Text := strings.Repeat("ab", 32)
	index := []byte(`{"byte_count":3,"chunk_byte_count":1,"chunks":[],"sha256":"` + sha256Text + `","version":1}`)
	chunk := bytes.Repeat([]byte{7}, 3*1024*1024)
	server := &computeModelTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/compute/models/" + sha256Text + "/index":
			w.Write(index)
		case "/compute/models/" + sha256Text + "/chunks/2":
			w.Write(chunk)
		default:
			http.NotFound(w, r)
		}
	}}
	_, api := newTestApi(t, server)
	api.SetByJwt(dpopTestStrictToken(t))

	gotIndex, err := api.ComputeModelIndexSyncWithContext(t.Context(), sha256Text)
	if err != nil || !bytes.Equal(gotIndex, index) {
		t.Fatalf("the index: %q %v", gotIndex, err)
	}
	gotChunk, err := api.ComputeModelChunkSyncWithContext(t.Context(), sha256Text, 2)
	if err != nil || !bytes.Equal(gotChunk, chunk) {
		t.Fatalf("chunk 2: %d bytes, %v; want its %d, past the strategy's own bound", len(gotChunk), err, len(chunk))
	}
	results := make(chan []byte, 2)
	api.ComputeModelIndex(sha256Text, connect.NewApiCallback(func(result []byte, err error) {
		if err != nil {
			t.Errorf("the index by callback: %v", err)
		}
		results <- result
	}))
	api.ComputeModelChunk(sha256Text, 2, connect.NewApiCallback(func(result []byte, err error) {
		if err != nil {
			t.Errorf("chunk 2 by callback: %v", err)
		}
		results <- result
	}))
	for range 2 {
		result := <-results
		if !bytes.Equal(result, index) && !bytes.Equal(result, chunk) {
			t.Fatalf("a callback answered %d bytes", len(result))
		}
	}

	seen := server.seen()
	if len(seen) != 4 {
		t.Fatalf("the server saw %d requests: %v", len(seen), seen)
	}
	for _, request := range seen {
		if request.method != http.MethodGet || request.rawQuery != "" || request.authorization != "" || request.cookie != "" || request.clientInfo != "" {
			t.Fatalf("a model route was sent %+v", request)
		}
		if request.path != "/compute/models/"+sha256Text+"/index" && request.path != "/compute/models/"+sha256Text+"/chunks/2" {
			t.Fatalf("a model route was sent to %s", request.path)
		}
	}
}

// A SHA-256 that is not 64 lowercase hex digits, one that would leave the route's path, and a chunk below 0 are
// refused before anything is sent.
func TestComputeModelApiRefusesMalformedRequestsUnsent(t *testing.T) {
	server := &computeModelTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}}
	_, api := newTestApi(t, server)
	sha256Text := strings.Repeat("0f", 32)
	for _, malformed := range []string{strings.ToUpper(sha256Text), sha256Text[:63], "../../auth/refresh/" + sha256Text[19:], "", sha256Text + "?x=1"} {
		if _, err := api.ComputeModelIndexSyncWithContext(t.Context(), malformed); !errors.Is(err, ErrComputeModelRequestMalformed) {
			t.Fatalf("the index of %q: %v", malformed, err)
		}
		if _, err := api.ComputeModelChunkSyncWithContext(t.Context(), malformed, 0); !errors.Is(err, ErrComputeModelRequestMalformed) {
			t.Fatalf("a chunk of %q: %v", malformed, err)
		}
	}
	if _, err := api.ComputeModelChunkSyncWithContext(t.Context(), sha256Text, -1); !errors.Is(err, ErrComputeModelRequestMalformed) {
		t.Fatalf("chunk -1: %v", err)
	}
	if seen := server.seen(); len(seen) != 0 {
		t.Fatalf("sent %v", seen)
	}
}

// An answer past its settings' bound is refused, whether its length is declared or streamed without one; within the
// bound it is read whole; a non-positive bound refuses every answer unsent.
func TestComputeModelApiBoundsTheAnswer(t *testing.T) {
	sha256Text := strings.Repeat("12", 32)
	body := bytes.Repeat([]byte{1}, 2048)
	server := &computeModelTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chunks/1") {
			// no Content-Length: the body streams
			w.(http.Flusher).Flush()
		}
		w.Write(body)
	}}
	_, api := newTestApi(t, server)
	settings := DefaultComputeModelApiSettings()
	settings.IndexMaxByteCount = 1024
	settings.ChunkMaxByteCount = 1024
	if _, err := api.ComputeModelIndexSyncWithSettings(t.Context(), settings, sha256Text); !errors.Is(err, ErrComputeModelAnswerTooLarge) {
		t.Fatalf("an index past its bound: %v", err)
	}
	for _, n := range []int{0, 1} {
		if _, err := api.ComputeModelChunkSyncWithSettings(t.Context(), settings, sha256Text, n); !errors.Is(err, ErrComputeModelAnswerTooLarge) {
			t.Fatalf("chunk %d past its bound: %v", n, err)
		}
	}
	settings.ChunkMaxByteCount = 2048
	if chunk, err := api.ComputeModelChunkSyncWithSettings(t.Context(), settings, sha256Text, 1); err != nil || len(chunk) != 2048 {
		t.Fatalf("a streamed chunk within its bound: %d %v", len(chunk), err)
	}
	sentCount := len(server.seen())
	settings.IndexMaxByteCount = 0
	if _, err := api.ComputeModelIndexSyncWithSettings(t.Context(), settings, sha256Text); !errors.Is(err, ErrComputeModelAnswerTooLarge) {
		t.Fatalf("no bound: %v", err)
	}
	if len(server.seen()) != sentCount {
		t.Fatal("a request with no bound was sent")
	}
}

// A refusal is a *connect.HttpStatusError carrying its status and body, as connect's raw functions answer, so a
// caller can tell a file not served (404) from a limit (429); a redirect is not followed.
func TestComputeModelApiRefusalIsAStatusError(t *testing.T) {
	sha256Text := strings.Repeat("34", 32)
	server := &computeModelTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/index"):
			w.Header().Set("Retry-After", "40")
			http.Error(w, "Too many model requests from this address; retry after Retry-After.", http.StatusTooManyRequests)
		case strings.HasSuffix(r.URL.Path, "/chunks/9"):
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			http.Error(w, "No such model file or chunk is served.", http.StatusNotFound)
		}
	}}
	_, api := newTestApi(t, server)
	_, err := api.ComputeModelIndexSyncWithContext(t.Context(), sha256Text)
	var statusError *connect.HttpStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a limited index: %v", err)
	}
	_, err = api.ComputeModelChunkSyncWithContext(t.Context(), sha256Text, 0)
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusNotFound || !strings.Contains(string(statusError.Body), "No such model file") {
		t.Fatalf("an unserved chunk: %v", err)
	}
	_, err = api.ComputeModelChunkSyncWithContext(t.Context(), sha256Text, 9)
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusFound {
		t.Fatalf("a redirect: %v", err)
	}
	for _, request := range server.seen() {
		if request.path == "/elsewhere" {
			t.Fatal("the redirect was followed")
		}
	}
}

// A test's installed transport carries the request instead, handed no credential either.
func TestComputeModelApiTakesAnInstalledTransport(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.test")
	api.SetByJwt(dpopTestStrictToken(t))
	requestsLock := sync.Mutex{}
	requests := []string{}
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		if !strings.Contains(requestUrl, "/compute/models/") {
			// the Api's own traffic, as a token refresh, is none of this test's
			return nil, errors.New("not a model route")
		}
		if byJwt != "" {
			t.Errorf("handed a credential: %q", byJwt)
		}
		requestsLock.Lock()
		defer requestsLock.Unlock()
		requests = append(requests, requestUrl)
		return []byte("chunk"), nil
	})
	sha256Text := strings.Repeat("56", 32)
	if chunk, err := api.ComputeModelChunkSyncWithContext(t.Context(), sha256Text, 12); err != nil || string(chunk) != "chunk" {
		t.Fatalf("%q %v", chunk, err)
	}
	requestsLock.Lock()
	defer requestsLock.Unlock()
	if len(requests) != 1 || requests[0] != "https://api.test/compute/models/"+sha256Text+"/chunks/12" {
		t.Fatalf("requests = %v", requests)
	}
}
