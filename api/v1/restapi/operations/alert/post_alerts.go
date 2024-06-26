// Code generated by go-swagger; DO NOT EDIT.

package alert

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the generate command

import (
	"net/http"

	"github.com/go-openapi/runtime/middleware"
)

// PostAlertsHandlerFunc turns a function with the right signature into a post alerts handler
type PostAlertsHandlerFunc func(PostAlertsParams) middleware.Responder

// Handle executing the request and returning a response
func (fn PostAlertsHandlerFunc) Handle(params PostAlertsParams) middleware.Responder {
	return fn(params)
}

// PostAlertsHandler interface for that can handle valid post alerts params
type PostAlertsHandler interface {
	Handle(PostAlertsParams) middleware.Responder
}

// NewPostAlerts creates a new http.Handler for the post alerts operation
func NewPostAlerts(ctx *middleware.Context, handler PostAlertsHandler) *PostAlerts {
	return &PostAlerts{Context: ctx, Handler: handler}
}

/*
	PostAlerts swagger:route POST /alerts alert postAlerts

Create new Alerts
*/
type PostAlerts struct {
	Context *middleware.Context
	Handler PostAlertsHandler
}

func (o *PostAlerts) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	route, rCtx, _ := o.Context.RouteInfo(r)
	if rCtx != nil {
		*r = *rCtx
	}
	var Params = NewPostAlertsParams()
	if err := o.Context.BindValidRequest(r, route, &Params); err != nil { // bind params
		o.Context.Respond(rw, r, route.Produces, route, err)
		return
	}

	res := o.Handler.Handle(Params) // actually handle the request
	o.Context.Respond(rw, r, route.Produces, route, res)

}
