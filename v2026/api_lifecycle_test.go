// API lifecycle tests pin synchronous refresh-worker ownership separately
// from callback-safe cancellation.
package sdk

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A canceled refresh that has not unwound is still API-owned. Completion is
// published only after the admitted request function returns.
func TestApiCloseAndWaitJoinsRefreshWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	strategy := newTestClientStrategy(ctx)
	api := NewApi(ctx, strategy, "http://unused.invalid")
	requestEntered := make(chan struct{})
	requestCanceled := make(chan struct{})
	requestRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(requestRelease)
		})
	}
	t.Cleanup(func() {
		release()
		api.Close()
		cancel()
		strategy.Close()
		_ = api.CloseAndWait(context.Background())
	})
	api.setHttpGetRaw(func(requestCtx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		close(requestEntered)
		<-requestCtx.Done()
		close(requestCanceled)
		<-requestRelease
		return nil, requestCtx.Err()
	})
	api.SetByJwt(testingRefreshableJwt(t))
	api.StartJwtRefresh()

	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-requestEntered:
	}
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- api.CloseAndWait(context.Background())
	}()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-requestCanceled:
	}
	select {
	case err := <-closeResult:
		t.Fatalf("API close returned before refresh request cleanup: %v", err)
	default:
	}
	release()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	}
}
