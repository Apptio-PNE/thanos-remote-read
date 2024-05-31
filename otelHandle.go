package main

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/label"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/semconv"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName   = "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	RequestCount          = "http.server.request_count"           // Incoming request count total
	RequestContentLength  = "http.server.request_content_length"  // Incoming request bytes total
	ResponseContentLength = "http.server.response_content_length" // Incoming response bytes total
	ServerLatency         = "http.server.duration"                // Incoming end to end duration, microseconds

	ReadBytesKey  = label.Key("http.read_bytes")  // if anything was read from the request body, the total number of bytes read
	ReadErrorKey  = label.Key("http.read_error")  // If an error occurred while reading a request, the string of the error (io.EOF is not recorded)
	WroteBytesKey = label.Key("http.wrote_bytes") // if anything was written to the response writer, the total number of bytes written
	WriteErrorKey = label.Key("http.write_error") // if an error occurred while writing a reply, the string of the error (io.EOF is not recorded)
)

// Filter is a predicate used to determine whether a given http.request should
// be traced. A Filter must return true if the request should be traced.
type Filter func(*http.Request) bool

type Handler struct {
	operation string
	handler   http.Handler

	tracer            trace.Tracer
	meter             metric.Meter
	propagators       propagation.TextMapPropagator
	spanStartOptions  []trace.SpanOption
	readEvent         bool
	writeEvent        bool
	filters           []Filter
	spanNameFormatter func(string, *http.Request) string
	counters          map[string]metric.Int64Counter
	valueRecorders    map[string]metric.Int64ValueRecorder
}

// NewHandler wraps the passed handler, functioning like middleware, in a span
// named after the operation and with any provided Options.
func NewHandler(handler http.Handler, operation string, opts ...Option) http.Handler {
	h := Handler{
		handler:   handler,
		operation: operation,
	}

	defaultOpts := []Option{
		WithSpanOptions(trace.WithSpanKind(trace.SpanKindServer)),
		WithSpanNameFormatter(defaultHandlerFormatter),
	}

	c := newConfig(append(defaultOpts, opts...)...)
	h.configure(c)
	h.createMeasures()

	return &h
}

func (h *Handler) configure(c *config) {
	h.tracer = c.Tracer
	h.meter = c.Meter
	h.propagators = c.Propagators
	h.spanStartOptions = c.SpanStartOptions
	h.readEvent = c.ReadEvent
	h.writeEvent = c.WriteEvent
	h.filters = c.Filters
	h.spanNameFormatter = c.SpanNameFormatter
}

func handleErr(err error) {
	if err != nil {
		otel.Handle(err)
	}
}

func (h *Handler) createMeasures() {
	h.counters = make(map[string]metric.Int64Counter)
	h.valueRecorders = make(map[string]metric.Int64ValueRecorder)

	requestBytesCounter, err := h.meter.NewInt64Counter(RequestContentLength)
	handleErr(err)

	responseBytesCounter, err := h.meter.NewInt64Counter(ResponseContentLength)
	handleErr(err)

	serverLatencyMeasure, err := h.meter.NewInt64ValueRecorder(ServerLatency)
	handleErr(err)

	h.counters[RequestContentLength] = requestBytesCounter
	h.counters[ResponseContentLength] = responseBytesCounter
	h.valueRecorders[ServerLatency] = serverLatencyMeasure
}

// ServeHTTP serves HTTP requests (http.Handler)
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestStartTime := time.Now()
	for _, f := range h.filters {
		if !f(r) {
			// Simply pass through to the handler if a filter rejects the request
			h.handler.ServeHTTP(w, r)
			return
		}
	}

	opts := append([]trace.SpanOption{
		trace.WithAttributes(semconv.NetAttributesFromHTTPRequest("tcp", r)...),
		trace.WithAttributes(semconv.EndUserAttributesFromHTTPRequest(r)...),
		trace.WithAttributes(semconv.HTTPServerAttributesFromHTTPRequest(h.operation, "", r)...),
	}, h.spanStartOptions...) // start with the configured options

	ctx := h.propagators.Extract(r.Context(), r.Header)
	ctx, span := h.tracer.Start(ctx, h.spanNameFormatter(h.operation, r), opts...)
	defer span.End()

	readRecordFunc := func(int64) {}
	if h.readEvent {
		readRecordFunc = func(n int64) {
			span.AddEvent("read", trace.WithAttributes(ReadBytesKey.Int64(n)))
		}
	}

	var bw bodyWrapper
	// if request body is nil we don't want to mutate the body as it will affect
	// the identity of it in a unforeseeable way because we assert ReadCloser
	// fullfills a certain interface and it is indeed nil.
	if r.Body != nil {
		bw.ReadCloser = r.Body
		bw.record = readRecordFunc
		r.Body = &bw
	}

	writeRecordFunc := func(int64) {}
	if h.writeEvent {
		writeRecordFunc = func(n int64) {
			span.AddEvent("write", trace.WithAttributes(WroteBytesKey.Int64(n)))
		}
	}

	rww := &respWriterWrapper{ResponseWriter: w, record: writeRecordFunc, ctx: ctx, props: h.propagators}

	// Wrap w to use our ResponseWriter methods while also exposing
	// other interfaces that w may implement (http.CloseNotifier,
	// http.Flusher, http.Hijacker, http.Pusher, io.ReaderFrom).

	w = httpsnoop.Wrap(w, httpsnoop.Hooks{
		Header: func(httpsnoop.HeaderFunc) httpsnoop.HeaderFunc {
			return rww.Header
		},
		Write: func(httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return rww.Write
		},
		WriteHeader: func(httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return rww.WriteHeader
		},
	})

	labeler := &Labeler{}
	ctx = injectLabeler(ctx, labeler)

	h.handler.ServeHTTP(w, r.WithContext(ctx))

	setAfterServeAttributes(span, bw.read, rww.written, rww.statusCode, bw.err, rww.err)

	// Add metrics
	labels := append(labeler.Get(), semconv.HTTPServerMetricAttributesFromHTTPRequest(h.operation, r)...)
	h.counters[RequestContentLength].Add(ctx, bw.read, labels...)
	h.counters[ResponseContentLength].Add(ctx, rww.written, labels...)

	elapsedTime := time.Since(requestStartTime).Microseconds()

	h.valueRecorders[ServerLatency].Record(ctx, elapsedTime, labels...)
}

func setAfterServeAttributes(span trace.Span, read, wrote int64, statusCode int, rerr, werr error) {
	labels := []label.KeyValue{}

	// TODO: Consider adding an event after each read and write, possibly as an
	// option (defaulting to off), so as to not create needlessly verbose spans.
	if read > 0 {
		labels = append(labels, ReadBytesKey.Int64(read))
	}
	if rerr != nil && rerr != io.EOF {
		labels = append(labels, ReadErrorKey.String(rerr.Error()))
	}
	if wrote > 0 {
		labels = append(labels, WroteBytesKey.Int64(wrote))
	}
	if statusCode > 0 {
		labels = append(labels, semconv.HTTPAttributesFromHTTPStatusCode(statusCode)...)
		span.SetStatus(semconv.SpanStatusFromHTTPStatusCode(statusCode))
	}
	if werr != nil && werr != io.EOF {
		labels = append(labels, WriteErrorKey.String(werr.Error()))
	}
	span.SetAttributes(labels...)
}

// newConfig creates a new config struct and applies opts to it.
func newConfig(opts ...Option) *config {
	c := &config{
		Propagators:    otel.GetTextMapPropagator(),
		TracerProvider: otel.GetTracerProvider(),
		MeterProvider:  otel.GetMeterProvider(),
	}
	for _, opt := range opts {
		opt.Apply(c)
	}

	c.Tracer = c.TracerProvider.Tracer(
		instrumentationName,
		trace.WithInstrumentationVersion(contrib.SemVersion()),
	)
	c.Meter = c.MeterProvider.Meter(
		instrumentationName,
		metric.WithInstrumentationVersion(contrib.SemVersion()),
	)

	return c
}

func defaultHandlerFormatter(operation string, _ *http.Request) string {
	return operation
}

// WithSpanNameFormatter takes a function that will be called on every
// request and the returned string will become the Span Name
func WithSpanNameFormatter(f func(operation string, r *http.Request) string) Option {
	return OptionFunc(func(c *config) {
		c.SpanNameFormatter = f
	})
}

// WithSpanOptions configures an additional set of
// trace.SpanOptions, which are applied to each new span.
func WithSpanOptions(opts ...trace.SpanOption) Option {
	return OptionFunc(func(c *config) {
		c.SpanStartOptions = append(c.SpanStartOptions, opts...)
	})
}

// config represents the configuration options available for the http.Handler
// and http.Transport types.
type config struct {
	Tracer            trace.Tracer
	Meter             metric.Meter
	Propagators       propagation.TextMapPropagator
	SpanStartOptions  []trace.SpanOption
	ReadEvent         bool
	WriteEvent        bool
	Filters           []Filter
	SpanNameFormatter func(string, *http.Request) string

	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// Option Interface used for setting *optional* config properties
type Option interface {
	Apply(*config)
}

// OptionFunc provides a convenience wrapper for simple Options
// that can be represented as functions.
type OptionFunc func(*config)

func (o OptionFunc) Apply(c *config) {
	o(c)
}

var _ io.ReadCloser = &bodyWrapper{}

// bodyWrapper wraps a http.Request.Body (an io.ReadCloser) to track the number
// of bytes read and the last error
type bodyWrapper struct {
	io.ReadCloser
	record func(n int64) // must not be nil

	read int64
	err  error
}

func (w *bodyWrapper) Read(b []byte) (int, error) {
	n, err := w.ReadCloser.Read(b)
	n1 := int64(n)
	w.read += n1
	w.err = err
	w.record(n1)
	return n, err
}

func (w *bodyWrapper) Close() error {
	return w.ReadCloser.Close()
}

var _ http.ResponseWriter = &respWriterWrapper{}

// respWriterWrapper wraps a http.ResponseWriter in order to track the number of
// bytes written, the last error, and to catch the returned statusCode
// TODO: The wrapped http.ResponseWriter doesn't implement any of the optional
// types (http.Hijacker, http.Pusher, http.CloseNotifier, http.Flusher, etc)
// that may be useful when using it in real life situations.
type respWriterWrapper struct {
	http.ResponseWriter
	record func(n int64) // must not be nil

	// used to inject the header
	ctx context.Context

	props propagation.TextMapPropagator

	written     int64
	statusCode  int
	err         error
	wroteHeader bool
}

func (w *respWriterWrapper) Header() http.Header {
	return w.ResponseWriter.Header()
}

func (w *respWriterWrapper) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	n1 := int64(n)
	w.record(n1)
	w.written += n1
	w.err = err
	return n, err
}

func (w *respWriterWrapper) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.statusCode = statusCode
	w.props.Inject(w.ctx, w.Header())
	w.ResponseWriter.WriteHeader(statusCode)
}

// Labeler is used to allow instrumented HTTP handlers to add custom labels to
// the metrics recorded by the net/http instrumentation.
type Labeler struct {
	mu     sync.Mutex
	labels []label.KeyValue
}

// Add labels to a Labeler.
func (l *Labeler) Add(ls ...label.KeyValue) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.labels = append(l.labels, ls...)
}

// Labels returns a copy of the labels added to the Labeler.
func (l *Labeler) Get() []label.KeyValue {
	l.mu.Lock()
	defer l.mu.Unlock()
	ret := make([]label.KeyValue, len(l.labels))
	copy(ret, l.labels)
	return ret
}

type labelerContextKeyType int

const lablelerContextKey labelerContextKeyType = 0

func injectLabeler(ctx context.Context, l *Labeler) context.Context {
	return context.WithValue(ctx, lablelerContextKey, l)
}
