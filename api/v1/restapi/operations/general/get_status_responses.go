// Code generated by go-swagger; DO NOT EDIT.

package general

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the swagger generate command

import (
	"net/http"

	"github.com/go-openapi/runtime"

	"github.com/prometheus/alertmanager/api/v1/models"
)

// GetStatusOKCode is the HTTP code returned for type GetStatusOK
const GetStatusOKCode int = 200

/*
GetStatusOK Get status response

swagger:response getStatusOK
*/
type GetStatusOK struct {

	/*
	  In: Body
	*/
	Payload *models.AlertmanagerStatus `json:"body,omitempty"`
}

// NewGetStatusOK creates GetStatusOK with default headers values
func NewGetStatusOK() *GetStatusOK {

	return &GetStatusOK{}
}

// WithPayload adds the payload to the get status o k response
func (o *GetStatusOK) WithPayload(payload *models.AlertmanagerStatus) *GetStatusOK {
	o.Payload = payload
	return o
}

// SetPayload sets the payload to the get status o k response
func (o *GetStatusOK) SetPayload(payload *models.AlertmanagerStatus) {
	o.Payload = payload
}

// WriteResponse to the client
func (o *GetStatusOK) WriteResponse(rw http.ResponseWriter, producer runtime.Producer) {

	rw.WriteHeader(200)
	if o.Payload != nil {
		payload := o.Payload
		if err := producer.Produce(rw, payload); err != nil {
			panic(err) // let the recovery middleware deal with this
		}
	}
}