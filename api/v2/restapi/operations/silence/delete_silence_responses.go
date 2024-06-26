// Code generated by go-swagger; DO NOT EDIT.

package silence

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the swagger generate command

import (
	"net/http"

	"github.com/go-openapi/runtime"
)

// DeleteSilenceOKCode is the HTTP code returned for type DeleteSilenceOK
const DeleteSilenceOKCode int = 200

/*
DeleteSilenceOK Delete silence response

swagger:response deleteSilenceOK
*/
type DeleteSilenceOK struct {
}

// NewDeleteSilenceOK creates DeleteSilenceOK with default headers values
func NewDeleteSilenceOK() *DeleteSilenceOK {

	return &DeleteSilenceOK{}
}

// WriteResponse to the client
func (o *DeleteSilenceOK) WriteResponse(rw http.ResponseWriter, producer runtime.Producer) {

	rw.Header().Del(runtime.HeaderContentType) //Remove Content-Type on empty responses

	rw.WriteHeader(200)
}

// DeleteSilenceNotFoundCode is the HTTP code returned for type DeleteSilenceNotFound
const DeleteSilenceNotFoundCode int = 404

/*
DeleteSilenceNotFound A silence with the specified ID was not found

swagger:response deleteSilenceNotFound
*/
type DeleteSilenceNotFound struct {
}

// NewDeleteSilenceNotFound creates DeleteSilenceNotFound with default headers values
func NewDeleteSilenceNotFound() *DeleteSilenceNotFound {

	return &DeleteSilenceNotFound{}
}

// WriteResponse to the client
func (o *DeleteSilenceNotFound) WriteResponse(rw http.ResponseWriter, producer runtime.Producer) {

	rw.Header().Del(runtime.HeaderContentType) //Remove Content-Type on empty responses

	rw.WriteHeader(404)
}

// DeleteSilenceInternalServerErrorCode is the HTTP code returned for type DeleteSilenceInternalServerError
const DeleteSilenceInternalServerErrorCode int = 500

/*
DeleteSilenceInternalServerError Internal server error

swagger:response deleteSilenceInternalServerError
*/
type DeleteSilenceInternalServerError struct {

	/*
	  In: Body
	*/
	Payload string `json:"body,omitempty"`
}

// NewDeleteSilenceInternalServerError creates DeleteSilenceInternalServerError with default headers values
func NewDeleteSilenceInternalServerError() *DeleteSilenceInternalServerError {

	return &DeleteSilenceInternalServerError{}
}

// WithPayload adds the payload to the delete silence internal server error response
func (o *DeleteSilenceInternalServerError) WithPayload(payload string) *DeleteSilenceInternalServerError {
	o.Payload = payload
	return o
}

// SetPayload sets the payload to the delete silence internal server error response
func (o *DeleteSilenceInternalServerError) SetPayload(payload string) {
	o.Payload = payload
}

// WriteResponse to the client
func (o *DeleteSilenceInternalServerError) WriteResponse(rw http.ResponseWriter, producer runtime.Producer) {

	rw.WriteHeader(500)
	payload := o.Payload
	if err := producer.Produce(rw, payload); err != nil {
		panic(err) // let the recovery middleware deal with this
	}
}
