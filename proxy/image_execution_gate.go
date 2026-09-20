package proxy

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type imageExecutionGate struct{ slots chan struct{} }
type imageExecutionContextKey struct{}

var processImageGate atomic.Pointer[imageExecutionGate]

// Configure once before accepting traffic. Shared by all keys and both direct
// image endpoints and durable workers, including preprocessing/postprocessing.
func ConfigureImageExecutionLimit(limit int) {
	if limit <= 0 {
		processImageGate.Store(nil)
		return
	}
	processImageGate.Store(&imageExecutionGate{slots: make(chan struct{}, limit)})
}

func AcquireImageExecution(ctx context.Context) (context.Context, func(), error) {
	gate := processImageGate.Load()
	if gate == nil {
		return ctx, func() {}, nil
	}
	select {
	case gate.slots <- struct{}{}:
		return context.WithValue(ctx, imageExecutionContextKey{}, true), func() { <-gate.slots }, nil
	case <-ctx.Done():
		return ctx, nil, ctx.Err()
	}
}

func admitDirectImageExecution(c *gin.Context) (func(), bool) {
	if inherited, _ := c.Request.Context().Value(imageExecutionContextKey{}).(bool); inherited {
		return func() {}, true
	}
	memoryGate := processImagePipeline.Load()
	if memoryGate != nil {
		select {
		case memoryGate.slots <- struct{}{}:
		default:
			c.Header("Retry-After", "2")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "Image processing busy; use /v1/images/jobs"}})
			return nil, false
		}
	}
	releaseMemory := func() {
		if memoryGate != nil {
			<-memoryGate.slots
		}
	}
	gate := processImageGate.Load()
	if gate == nil {
		return releaseMemory, true
	}
	select {
	case gate.slots <- struct{}{}:
		return func() { <-gate.slots; releaseMemory() }, true
	default:
		releaseMemory()
		c.Header("Retry-After", "2")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "Image workers are busy; submit /v1/images/jobs to queue the request", "type": "rate_limit_error"}})
		return nil, false
	}
}

// Image jobs deliberately bypass the generic API-key concurrency limiter.
func (h *Handler) TryAcquireImageJobKey(_ *database.APIKeyRow) (func(), bool) {
	// Image jobs intentionally do not consume API-key concurrency slots. The
	// image account scheduler and upstream health/cooldown handling remain the
	// only execution safeguards for this workload.
	return func() {}, true
}
