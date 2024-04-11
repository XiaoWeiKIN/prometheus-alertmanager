// Code generated by go-swagger; DO NOT EDIT.

package receiver

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the generate command

import (
	"net/http"

	"github.com/go-openapi/runtime/middleware"
)

// GetReceiversHandlerFunc turns a function with the right signature into a get receivers handler
type GetReceiversHandlerFunc func(GetReceiversParams) middleware.Responder

// Handle executing the request and returning a response
func (fn GetReceiversHandlerFunc) Handle(params GetReceiversParams) middleware.Responder {
	return fn(params)
}

// GetReceiversHandler interface for that can handle valid get receivers params
type GetReceiversHandler interface {
	Handle(GetReceiversParams) middleware.Responder
}

// NewGetReceivers creates a new http.Handler for the get receivers operation
func NewGetReceivers(ctx *middleware.Context, handler GetReceiversHandler) *GetReceivers {
	return &GetReceivers{Context: ctx, Handler: handler}
}

/*
	GetReceivers swagger:route GET /receivers receiver getReceivers

Get list of all receivers (name of notification integrations)
*/
type GetReceivers struct {
	Context *middleware.Context
	Handler GetReceiversHandler
}

func (o *GetReceivers) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	route, rCtx, _ := o.Context.RouteInfo(r)
	if rCtx != nil {
		*r = *rCtx
	}
	var Params = NewGetReceiversParams()
	if err := o.Context.BindValidRequest(r, route, &Params); err != nil { // bind params
		o.Context.Respond(rw, r, route.Produces, route, err)
		return
	}

	res := o.Handler.Handle(Params) // actually handle the request
	o.Context.Respond(rw, r, route.Produces, route, res)

}
