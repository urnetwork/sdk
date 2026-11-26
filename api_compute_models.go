package sdk

// The mirror of api's model routes (DESIGN-COMPUTE §14.3, §6.5, CO-49; DESIGN CONVENTIONS §8): a local model file's
// chunk index, GET /compute/models/{sha256}/index, and its chunks, GET /compute/models/{sha256}/chunks/{n}, bytes in
// and out. The routes take no token and read none, so these send none, nor a cookie or the client's info: the api
// learns a file and an address and never a member. Neither checks what it fetched, which is the caller's to do against
// its pins (the index against the signed manifest's index_sha256, each chunk against the index); the one downloader
// every Go host uses is the sidecar's (INFER-M11-11), which keeps those checks, its resume and its fallback. These are
// for an app or a tool that fetches one object.
//
// A chunk is up to 16 MiB, past what the client strategy reads into memory, so these read through a transport of
// their own on the strategy's dial and TLS settings, never net/http's default one, whose sockets would follow a tunnel's
// route back into this process, each answer bounded by ComputeModelApiSettings.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/urnetwork/connect"
)

// The bounds of what the model routes' mirror reads (QD-702).
type ComputeModelApiSettings struct {
	// The largest index read, in bytes: 1 MiB, the api's own bound on an index it serves (sidecar/handler's
	// ComputeModelRouteSettings), itself INFER-M11-10's ModelIndexMaxByteCount once that lands. A larger answer is
	// refused, never cut. Non-positive refuses every index.
	IndexMaxByteCount int64
	// The largest chunk read, in bytes: 64 MiB, the api's bound on a chunk a served index may name, four times §6.5's
	// 16 MiB. As IndexMaxByteCount for a larger answer or a non-positive bound.
	ChunkMaxByteCount int64
	// How long an index's fetch may take: 30 seconds, an index being at most a megabyte. Non-positive leaves only the
	// caller's context.
	IndexTimeout time.Duration
	// How long a chunk's fetch may take: 5 minutes, a 16 MiB chunk on a link under half a megabit a second, the api's
	// own write deadline for one. As IndexTimeout for a non-positive one.
	ChunkTimeout time.Duration
}

func DefaultComputeModelApiSettings() *ComputeModelApiSettings {
	return &ComputeModelApiSettings{
		IndexMaxByteCount: 1024 * 1024,
		ChunkMaxByteCount: 64 * 1024 * 1024,
		IndexTimeout:      30 * time.Second,
		ChunkTimeout:      5 * time.Minute,
	}
}

// A model route's answer: the object's bytes.
type ComputeModelBytesCallback connect.ApiCallback[[]byte]

// A request a model route would refuse, refused before it is sent: a SHA-256 that is not 64 lowercase hex digits, or
// a chunk number below 0.
var ErrComputeModelRequestMalformed = errors.New("a model route takes a file's SHA-256 in lowercase hex and a chunk number from 0")

// An answer past its settings' bound, refused unread past it.
var ErrComputeModelAnswerTooLarge = errors.New("the model route's answer is past its bound")

// Fetches the chunk index of the file whose SHA-256 is sha256Text.
func (self *Api) ComputeModelIndex(sha256Text string, callback ComputeModelBytesCallback) {
	runAsyncApiRequest[[]byte](callback, func(callback connect.ApiCallback[[]byte]) {
		index, err := self.ComputeModelIndexSyncWithSettings(self.ctx, DefaultComputeModelApiSettings(), sha256Text)
		callback.Result(index, err)
	})
}

// The blocking form.
//
//gomobile:noexport
func (self *Api) ComputeModelIndexSyncWithContext(ctx context.Context, sha256Text string) ([]byte, error) {
	return self.ComputeModelIndexSyncWithSettings(ctx, DefaultComputeModelApiSettings(), sha256Text)
}

//gomobile:noexport
func (self *Api) ComputeModelIndexSync(sha256Text string) ([]byte, error) {
	return self.ComputeModelIndexSyncWithContext(self.ctx, sha256Text)
}

// The blocking form under settings.
//
//gomobile:noexport
func (self *Api) ComputeModelIndexSyncWithSettings(ctx context.Context, settings *ComputeModelApiSettings, sha256Text string) ([]byte, error) {
	if !computeModelSha256Valid(sha256Text) {
		return nil, ErrComputeModelRequestMalformed
	}
	return self.computeModelGet(
		ctx,
		fmt.Sprintf("%s/compute/models/%s/index", self.apiUrl, sha256Text),
		settings.IndexMaxByteCount,
		settings.IndexTimeout,
	)
}

// Fetches chunk n of the file whose SHA-256 is sha256Text.
func (self *Api) ComputeModelChunk(sha256Text string, n int, callback ComputeModelBytesCallback) {
	runAsyncApiRequest[[]byte](callback, func(callback connect.ApiCallback[[]byte]) {
		chunk, err := self.ComputeModelChunkSyncWithSettings(self.ctx, DefaultComputeModelApiSettings(), sha256Text, n)
		callback.Result(chunk, err)
	})
}

// The blocking form.
//
//gomobile:noexport
func (self *Api) ComputeModelChunkSyncWithContext(ctx context.Context, sha256Text string, n int) ([]byte, error) {
	return self.ComputeModelChunkSyncWithSettings(ctx, DefaultComputeModelApiSettings(), sha256Text, n)
}

//gomobile:noexport
func (self *Api) ComputeModelChunkSync(sha256Text string, n int) ([]byte, error) {
	return self.ComputeModelChunkSyncWithContext(self.ctx, sha256Text, n)
}

// The blocking form under settings.
//
//gomobile:noexport
func (self *Api) ComputeModelChunkSyncWithSettings(ctx context.Context, settings *ComputeModelApiSettings, sha256Text string, n int) ([]byte, error) {
	if !computeModelSha256Valid(sha256Text) || n < 0 {
		return nil, ErrComputeModelRequestMalformed
	}
	return self.computeModelGet(
		ctx,
		fmt.Sprintf("%s/compute/models/%s/chunks/%d", self.apiUrl, sha256Text, n),
		settings.ChunkMaxByteCount,
		settings.ChunkTimeout,
	)
}

// One GET of a model route, with no credential, read to at most maxByteCount: through a test's transport where one is
// installed (setHttpGetRaw), else through a transport on this Api's client strategy made for the request and closed
// with it. A status other than 200 is a *connect.HttpStatusError, as connect's raw functions answer.
func (self *Api) computeModelGet(ctx context.Context, requestUrl string, maxByteCount int64, timeout time.Duration) ([]byte, error) {
	if maxByteCount <= 0 {
		return nil, ErrComputeModelAnswerTooLarge
	}
	if 0 < timeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	self.mutex.Lock()
	httpGetRaw := self.httpGetRaw
	clientStrategy := self.clientStrategy
	self.mutex.Unlock()

	if httpGetRaw != nil {
		// a test's transport, which is handed no credential either
		body, err := httpGetRaw(ctx, requestUrl, "")
		if err != nil {
			return nil, err
		}
		if maxByteCount < int64(len(body)) {
			return nil, ErrComputeModelAnswerTooLarge
		}
		return body, nil
	}

	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := &http.Client{
		// ends its idle connections as requestCtx does
		Transport: newApiDpopTransport(requestCtx, clientStrategy),
		// the api answers a model route itself; a redirect is none of its answers
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestUrl, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if maxByteCount < response.ContentLength {
		return nil, ErrComputeModelAnswerTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxByteCount+1))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, &connect.HttpStatusError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Body:       body,
		}
	}
	if maxByteCount < int64(len(body)) {
		return nil, ErrComputeModelAnswerTooLarge
	}
	if 0 <= response.ContentLength && response.ContentLength != int64(len(body)) {
		return nil, fmt.Errorf("the model route answered %d of its %d bytes", len(body), response.ContentLength)
	}
	return body, nil
}

// Whether text is a SHA-256 in lowercase hex.
func computeModelSha256Valid(text string) bool {
	if len(text) != 2*sha256.Size {
		return false
	}
	for i := 0; i < len(text); i += 1 {
		c := text[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
