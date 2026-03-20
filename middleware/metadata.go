package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------------------------------------------------

// MetadataOptions defines the configuration for the Metadata middleware.
type MetadataOptions struct {
	NeedRequestID    bool              // Whether to inject RequestID into response header.
	RequestIdKey     string            // Header key for RequestID.
	NeedReceiveTime  bool              // Whether to inject receive time into response header.
	ReceiveTimeKey   string            // Header key for ReceiveTime.
	NeedResponseTime bool              // Whether to inject response time into response header.
	ResponseTimeKey  string            // Header key for ResponseTime.
	Custom           map[string]string // Static custom headers.
}

// NewMetadata initializes the observability middleware.
// It iteratively merges multiple options (last-in-win).
func NewMetadata(opts ...*MetadataOptions) gin.HandlerFunc {
	// Initialize with defaults
	conf := &MetadataOptions{
		NeedRequestID:    true,
		RequestIdKey:     "X-Request-ID",
		NeedReceiveTime:  true,
		ReceiveTimeKey:   "X-Receive-Time",
		NeedResponseTime: true,
		ResponseTimeKey:  "X-Response-Time",
		Custom:           make(map[string]string),
	}

	for _, user := range opts {
		if user == nil {
			continue
		}
		conf.NeedRequestID = user.NeedRequestID
		conf.NeedReceiveTime = user.NeedReceiveTime
		conf.NeedResponseTime = user.NeedResponseTime

		if user.RequestIdKey != "" {
			conf.RequestIdKey = user.RequestIdKey
		}
		if user.ReceiveTimeKey != "" {
			conf.ReceiveTimeKey = user.ReceiveTimeKey
		}
		if user.ResponseTimeKey != "" {
			conf.ResponseTimeKey = user.ResponseTimeKey
		}

		if user.Custom != nil {
			for k, v := range user.Custom {
				conf.Custom[k] = v
			}
		}
	}

	engine := &metadataEngine{conf: conf}
	return engine.Handle
}

// ---------------------------------------------------------------------------------------------------------------------

// metadataEngine internal logic processor for metadata observability.
type metadataEngine struct {
	conf *MetadataOptions
}

// Handle processes the middleware logic.
func (e *metadataEngine) Handle(c *gin.Context) {
	start := time.Now()

	// 1. Context storage and Traceability
	c.Set(e.conf.ReceiveTimeKey, start)

	rid := c.GetHeader(e.conf.RequestIdKey)
	if rid == "" {
		rid = uuid.NewString()
	}
	c.Set(e.conf.RequestIdKey, rid)

	// 2. Header Visibility
	if e.conf.NeedRequestID {
		c.Header(e.conf.RequestIdKey, rid)
	}
	if e.conf.NeedReceiveTime {
		c.Header(e.conf.ReceiveTimeKey, start.Format(time.RFC3339Nano))
	}

	// 3. Custom Headers
	for k, v := range e.conf.Custom {
		c.Header(k, v)
		c.Set(k, v)
	}

	// 4. Wrap writer
	writer := &metadataWriter{
		ResponseWriter: c.Writer,
		conf:           e.conf,
		start:          start,
	}
	c.Writer = writer

	c.Next()

	// Final injection for empty responses
	writer.inject()
}

// ---------------------------------------------------------------------------------------------------------------------

// metadataWriter wraps gin.ResponseWriter to intercept response streaming.
type metadataWriter struct {
	gin.ResponseWriter
	conf           *MetadataOptions
	start          time.Time
	headerInjected bool
}

// inject ensures metadata headers are set before any data is written.
func (w *metadataWriter) inject() {
	if w.headerInjected {
		return
	}
	w.headerInjected = true

	if !w.ResponseWriter.Written() {
		h := w.Header()
		if w.conf.NeedResponseTime {
			h.Set(w.conf.ResponseTimeKey, time.Now().Format(time.RFC3339Nano))
		}
	}
}

func (w *metadataWriter) WriteHeader(code int) {
	w.inject()
	w.ResponseWriter.WriteHeader(code)
}

func (w *metadataWriter) Write(data []byte) (int, error) {
	w.inject()
	return w.ResponseWriter.Write(data)
}

func (w *metadataWriter) WriteString(s string) (int, error) {
	w.inject()
	if ws, ok := w.ResponseWriter.(interface {
		WriteString(string) (int, error)
	}); ok {
		return ws.WriteString(s)
	}
	return w.ResponseWriter.Write([]byte(s))
}

func (w *metadataWriter) Flush() {
	w.inject()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *metadataWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.inject()
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *metadataWriter) Push(target string, opts *http.PushOptions) error {
	w.inject()
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *metadataWriter) ReadFrom(r io.Reader) (int64, error) {
	w.inject()
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}
