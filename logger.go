package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httplog/v2"
)

// All the code below is basically copied from httplog, except it's patching the bugs in there.
// We can delete all of this once https://github.com/go-chi/httplog/pull/34 is merged in httplog, I guess.

// newLogger does not support logging requestHeaders unlike the httplog one, but response headers are working now.
func newLogger(options httplog.Options) *httplog.Logger {
	logger := &httplog.Logger{}

	logger.Configure(options)

	slogger := slog.With(slog.Attr{Key: "service", Value: slog.StringValue("http-relay")})

	if !logger.Options.Concise && len(logger.Options.Tags) > 0 {
		group := []any{}
		for k, v := range logger.Options.Tags {
			group = append(group, slog.Attr{Key: k, Value: slog.StringValue(v)})
		}
		slogger = slogger.With(slog.Group("tags", group...))
	}

	logger.Logger = slogger
	return logger
}

func handleLogger(logger *httplog.Logger, skipPaths ...[]string) func(next http.Handler) http.Handler {
	return chi.Chain(
		middleware.RequestID,
		Handler(logger, skipPaths...),
		middleware.Recoverer,
	).Handler
}

type requestLogger struct {
	slog.Logger
	Options httplog.Options
}

func (l *requestLogger) NewLogEntry(r *http.Request) middleware.LogEntry {
	entry := &httplog.RequestLoggerEntry{Options: l.Options}
	msg := fmt.Sprintf("Request: %s %s", r.Method, r.URL.Path)

	entry.Logger = *l.Logger.With(requestLogFields(r, l.Options))

	if !l.Options.Concise {
		entry.Logger.Info(msg)
	}
	return entry
}

func requestLogFields(r *http.Request, options httplog.Options) slog.Attr {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	requestURL := fmt.Sprintf("%s://%s%s", scheme, r.Host, r.RequestURI)

	requestFields := []any{
		slog.Attr{Key: "url", Value: slog.StringValue(requestURL)},
		slog.Attr{Key: "method", Value: slog.StringValue(r.Method)},
		slog.Attr{Key: "path", Value: slog.StringValue(r.URL.Path)},
		slog.Attr{Key: "remoteIP", Value: slog.StringValue(r.RemoteAddr)},
		slog.Attr{Key: "proto", Value: slog.StringValue(r.Proto)},
	}
	if reqID := middleware.GetReqID(r.Context()); reqID != "" {
		requestFields = append(requestFields, slog.Attr{Key: "requestID", Value: slog.StringValue(reqID)})
	}

	return slog.Group("httpRequest", requestFields...)
}

func Handler(logger *httplog.Logger, optSkipPaths ...[]string) func(next http.Handler) http.Handler {
	var f middleware.LogFormatter = &requestLogger{*logger.Logger, logger.Options}

	skipPaths := map[string]struct{}{}
	if len(optSkipPaths) > 0 {
		for _, path := range optSkipPaths[0] {
			skipPaths[path] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			// Skip the logger if the path is in the skip list
			if len(skipPaths) > 0 {
				_, skip := skipPaths[r.URL.Path]
				if skip {
					next.ServeHTTP(w, r)
					return
				}
			}

			entry := f.NewLogEntry(r)
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			buf := newLimitBuffer(512)
			ww.Tee(buf)

			t1 := time.Now()
			defer func() {
				var respBody []byte
				if ww.Status() >= 400 {
					respBody, _ = io.ReadAll(buf)
				}
				entry.Write(ww.Status(), ww.BytesWritten(), ww.Header(), time.Since(t1), respBody)
			}()

			next.ServeHTTP(ww, middleware.WithLogEntry(r, entry))
		}
		return http.HandlerFunc(fn)
	}
}

// limitBuffer is used to pipe response body information from the
// response writer to a certain limit amount. The idea is to read
// a portion of the response body such as an error response so we
// may log it.
type limitBuffer struct {
	*bytes.Buffer
	limit int
}

func newLimitBuffer(size int) io.ReadWriter {
	return limitBuffer{
		Buffer: bytes.NewBuffer(make([]byte, 0, size)),
		limit:  size,
	}
}

func (b limitBuffer) Write(p []byte) (n int, err error) {
	if b.Buffer.Len() >= b.limit {
		return len(p), nil
	}
	limit := b.limit
	if len(p) < limit {
		limit = len(p)
	}
	return b.Buffer.Write(p[:limit])
}

func (b limitBuffer) Read(p []byte) (n int, err error) {
	return b.Buffer.Read(p)
}
