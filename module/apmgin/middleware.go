package apmgin

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/elastic/apm-agent-go"
	"github.com/elastic/apm-agent-go/module/apmhttp"
	"github.com/elastic/apm-agent-go/stacktrace"
)

func init() {
	stacktrace.RegisterLibraryPackage(
		"github.com/gin-gonic",
		"github.com/gin-contrib",
	)
}

// Middleware returns a new Gin middleware handler for tracing
// requests and reporting errors.
//
// This middleware will recover and report panics, so it can
// be used instead of the standard gin.Recovery middleware.
//
// By default, the middleware will use elasticapm.DefaultTracer.
// Use WithTracer to specify an alternative tracer.
func Middleware(engine *gin.Engine, o ...Option) gin.HandlerFunc {
	m := &middleware{engine: engine, tracer: elasticapm.DefaultTracer}
	for _, o := range o {
		o(m)
	}
	return m.handle
}

type middleware struct {
	engine *gin.Engine
	tracer *elasticapm.Tracer

	setRouteMapOnce sync.Once
	routeMap        map[string]map[string]routeInfo
}

type routeInfo struct {
	transactionName string // e.g. "GET /foo"
}

func (m *middleware) handle(c *gin.Context) {
	if !m.tracer.Active() {
		c.Next()
		return
	}
	m.setRouteMapOnce.Do(func() {
		routes := m.engine.Routes()
		rm := make(map[string]map[string]routeInfo)
		for _, r := range routes {
			mm := rm[r.Method]
			if mm == nil {
				mm = make(map[string]routeInfo)
				rm[r.Method] = mm
			}
			mm[r.Handler] = routeInfo{
				transactionName: r.Method + " " + r.Path,
			}
		}
		m.routeMap = rm
	})

	requestName := c.Request.Method
	handlerName := c.HandlerName()
	if routeInfo, ok := m.routeMap[c.Request.Method][handlerName]; ok {
		requestName = routeInfo.transactionName
	}
	tx := m.tracer.StartTransaction(requestName, "request")
	if tx.Ignored() {
		tx.Discard()
		c.Next()
		return
	}

	ctx := elasticapm.ContextWithTransaction(c.Request.Context(), tx)
	c.Request = apmhttp.RequestWithContext(ctx, c.Request)
	defer tx.End()

	body := m.tracer.CaptureHTTPRequestBody(c.Request)
	ginContext := ginContext{Handler: handlerName}
	defer func() {
		if v := recover(); v != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			e := m.tracer.Recovered(v, tx)
			e.Context.SetHTTPRequest(c.Request)
			e.Context.SetHTTPRequestBody(body)
			e.Send()
		}
		tx.Result = apmhttp.StatusCodeResult(c.Writer.Status())

		if tx.Sampled() {
			tx.Context.SetHTTPRequest(c.Request)
			tx.Context.SetHTTPRequestBody(body)
			tx.Context.SetHTTPStatusCode(c.Writer.Status())
			tx.Context.SetHTTPResponseHeaders(c.Writer.Header())
			tx.Context.SetHTTPResponseHeadersSent(c.Writer.Written())
			tx.Context.SetHTTPResponseFinished(!c.IsAborted())
			tx.Context.SetCustom("gin", ginContext)
		}

		for _, err := range c.Errors {
			e := m.tracer.NewError(err.Err)
			e.Context.SetHTTPRequest(c.Request)
			e.Context.SetHTTPRequestBody(body)
			e.Context.SetCustom("gin", ginContext)
			e.Transaction = tx
			e.Handled = true
			e.Send()
		}
	}()
	c.Next()
}

type ginContext struct {
	Handler string `json:"handler"`
}

// Option sets options for tracing.
type Option func(*middleware)

// WithTracer returns an Option which sets t as the tracer
// to use for tracing server requests.
func WithTracer(t *elasticapm.Tracer) Option {
	if t == nil {
		panic("t == nil")
	}
	return func(m *middleware) {
		m.tracer = t
	}
}
