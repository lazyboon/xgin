package middleware

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// DefaultSensitiveHeaders provides a baseline for common sensitive headers to be masked.
var DefaultSensitiveHeaders = []string{
	"authorization", "proxy-authorization", "cookie", "set-cookie", "www-authenticate",
}

// ---------------------------------------------------------------------------------------------------------------------

// RecoveryOptions defines the configuration for the recovery middleware.
type RecoveryOptions struct {
	LogCallback      func(c *gin.Context, msg string) // Callback for custom logging (e.g., Zap, Sentry)
	Handler          func(c *gin.Context, err any)    // Custom response handler after a panic
	StackSkip        int                              // Frames to skip in stack trace, default is 3
	MaxStack         int                              // Max frames to capture, default is 50
	SensitiveHeaders []string                         // Headers to be masked in logs
}

// NewRecovery returns a high-reliability recovery middleware with trace support.
func NewRecovery(options ...*RecoveryOptions) gin.HandlerFunc {
	conf := &RecoveryOptions{
		StackSkip: 3,
		MaxStack:  50,
	}

	for _, opt := range options {
		if opt == nil {
			continue
		}
		if opt.LogCallback != nil {
			conf.LogCallback = opt.LogCallback
		}
		if opt.Handler != nil {
			conf.Handler = opt.Handler
		}
		if opt.MaxStack > 0 {
			conf.MaxStack = opt.MaxStack
		}
		if opt.StackSkip > 0 {
			conf.StackSkip = opt.StackSkip
		}
		conf.SensitiveHeaders = opt.SensitiveHeaders
	}

	engine := &recoveryEngine{
		conf: conf,
		mask: make(map[string]struct{}, len(conf.SensitiveHeaders)),
	}
	for _, h := range conf.SensitiveHeaders {
		engine.mask[strings.ToLower(strings.TrimSpace(h))] = struct{}{}
	}

	return engine.Handle
}

// ---------------------------------------------------------------------------------------------------------------------

// recoveryEngine internal logic processor to isolate namespace.
type recoveryEngine struct {
	conf *RecoveryOptions
	mask map[string]struct{}
}

// Handle implements the gin.HandlerFunc interface.
func (e *recoveryEngine) Handle(c *gin.Context) {
	defer func() {
		if err := recover(); err != nil {
			// Double panic protection
			defer func() {
				if r := recover(); r != nil {
					_, _ = fmt.Fprintf(os.Stderr, "[Recovery] CRITICAL: Double panic: %v\n", r)
				}
			}()

			isBroken := e.isBrokenPipe(err)
			requestID := e.getRequestID(c)

			var b strings.Builder
			b.Grow(1024)

			e.writeLogHeader(&b, requestID, isBroken, err)
			e.writeRequestHeaders(&b, c)
			e.writeStackTrace(&b)

			logMsg := b.String()

			if e.conf.LogCallback != nil {
				e.conf.LogCallback(c, logMsg)
			}

			if isBroken {
				if errObj, ok := err.(error); ok {
					_ = c.Error(errObj)
				}
				c.Abort()
			} else if e.conf.Handler != nil {
				e.conf.Handler(c, err)
			} else {
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}
	}()
	c.Next()
}

func (e *recoveryEngine) getRequestID(c *gin.Context) string {
	if val, ok := c.Get("X-Request-ID"); ok {
		if s, ok := val.(string); ok && s != "" {
			return s
		}
	}
	if s := c.GetHeader("X-Request-ID"); s != "" {
		return s
	}
	return uuid.NewString()
}

func (e *recoveryEngine) writeLogHeader(b *strings.Builder, id string, isBroken bool, err any) {
	logType := "PANIC"
	if isBroken {
		logType = "BROKEN_PIPE"
	}
	b.WriteString("[")
	b.WriteString(time.Now().Format(time.RFC3339Nano))
	b.WriteString("] [")
	b.WriteString(logType)
	b.WriteString("] ID:")
	b.WriteString(id)
	b.WriteString(" | Err: ")
	b.WriteString(fmt.Sprint(err))
}

func (e *recoveryEngine) writeRequestHeaders(b *strings.Builder, c *gin.Context) {
	b.WriteString("\nHeaders: ")
	first := true
	for k, v := range c.Request.Header {
		if !first {
			b.WriteString(" | ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		if _, sensitive := e.mask[strings.ToLower(k)]; sensitive {
			b.WriteString("******")
		} else {
			b.WriteString(strings.Join(v, ","))
		}
		first = false
	}
}

func (e *recoveryEngine) writeStackTrace(b *strings.Builder) {
	b.WriteString("\nStack:\n")

	skip := e.conf.StackSkip
	maxDepth := e.conf.MaxStack
	if maxDepth <= 0 {
		maxDepth = 50
	}

	for i := skip; ; i++ {
		if (i - skip) >= maxDepth {
			b.WriteString("  ... (max depth reached)\n")
			break
		}

		pc, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}

		fnName := "???"
		if fn := runtime.FuncForPC(pc); fn != nil {
			fnName = fn.Name()
			if lastSlash := strings.LastIndex(fnName, "/"); lastSlash >= 0 {
				fnName = fnName[lastSlash+1:]
			}
		}

		shortFile := file
		if parts := strings.Split(file, "/"); len(parts) > 2 {
			shortFile = strings.Join(parts[len(parts)-2:], "/")
		}

		_, _ = fmt.Fprintf(b, "  %s:%d [%s]\n", shortFile, line, fnName)
	}
}

// isBrokenPipe checks if the error is related to a closed/broken network connection.
func (e *recoveryEngine) isBrokenPipe(err any) bool {
	if err == nil {
		return false
	}
	errObj, ok := err.(error)
	if !ok {
		s := strings.ToLower(fmt.Sprint(err))
		return strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset by peer")
	}

	if errors.Is(errObj, io.EOF) || errors.Is(errObj, io.ErrClosedPipe) {
		return true
	}

	var netOpErr *net.OpError
	if errors.As(errObj, &netOpErr) {
		var syscallErr *os.SyscallError
		if errors.As(netOpErr.Err, &syscallErr) {
			s := strings.ToLower(syscallErr.Error())
			return strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset by peer")
		}
	}
	return false
}
