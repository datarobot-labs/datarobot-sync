// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package drapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// APIError is a non-2xx response from the DataRobot API.
type APIError struct {
	Method    string
	Path      string
	Status    int
	Message   string
	ErrorName string
	// RequestID comes from the X-DataRobot-Request-ID header and is what support asks for.
	RequestID string
	// Body is the raw response, truncated, for the cases where the shape was unexpected.
	Body string
}

func (e *APIError) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s %s: HTTP %d", e.Method, e.Path, e.Status)

	if e.ErrorName != "" {
		fmt.Fprintf(&b, " (%s)", e.ErrorName)
	}

	switch {
	case e.Message != "":
		fmt.Fprintf(&b, ": %s", e.Message)
	case e.Body != "":
		fmt.Fprintf(&b, ": %s", e.Body)
	}

	if e.RequestID != "" {
		fmt.Fprintf(&b, " [request %s]", e.RequestID)
	}

	return b.String()
}

// errorEnvelope covers the shapes DataRobot returns for a failure. `message` is usually a
// string but is an object of field errors for validation failures, so it is decoded loosely.
type errorEnvelope struct {
	Message   json.RawMessage `json:"message"`
	Detail    string          `json:"detail"`
	ErrorName string          `json:"errorName"`
	Errors    json.RawMessage `json:"errors"`
}

// parseAPIError builds an APIError from a response whose body has already been read.
func parseAPIError(method, path string, resp *http.Response, body []byte) *APIError {
	apiErr := &APIError{
		Method:    method,
		Path:      path,
		Status:    resp.StatusCode,
		RequestID: resp.Header.Get("X-DataRobot-Request-ID"),
	}

	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		apiErr.Body = truncate(string(body), 512)

		return apiErr
	}

	apiErr.ErrorName = envelope.ErrorName
	apiErr.Message = flattenMessage(envelope.Message)

	if apiErr.Message == "" {
		apiErr.Message = envelope.Detail
	}

	if apiErr.Message == "" {
		apiErr.Body = truncate(string(body), 512)
	}

	if len(envelope.Errors) > 0 && string(envelope.Errors) != "null" {
		apiErr.Message = strings.TrimSpace(apiErr.Message + " " + truncate(string(envelope.Errors), 256))
	}

	return apiErr
}

// flattenMessage renders `message`, which is a string for most errors and a field→reason
// object for validation failures.
func flattenMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return truncate(string(raw), 256)
	}

	parts := make([]string, 0, len(fields))
	for field, reason := range fields {
		parts = append(parts, fmt.Sprintf("%s: %v", field, reason))
	}

	// Map iteration order is random; sorting keeps error text stable across runs.
	sortStrings(parts)

	return strings.Join(parts, "; ")
}

// StatusOf reports the HTTP status behind err, or 0 if err is not an APIError.
func StatusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}

	return 0
}

// IsNotFound reports whether err is a 404, or a 410 Gone — which is what a soft-deleted
// container returns and is, for every caller's purposes, the same thing.
func IsNotFound(err error) bool {
	status := StatusOf(err)

	return status == http.StatusNotFound || status == http.StatusGone
}

// IsAuth reports whether err is a credentials or permissions failure.
func IsAuth(err error) bool {
	status := StatusOf(err)

	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// IsConflict reports whether err is a 409, which for `fromStage` means a name in the stage
// already exists in the container and the entire commit was rejected.
func IsConflict(err error) bool {
	return StatusOf(err) == http.StatusConflict
}

// IsMissingFileName reports whether err is the API's response to naming a file that is not
// in the container. It is how a stale manifest surfaces: a pack it references has been
// compacted away.
func IsMissingFileName(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	return strings.Contains(apiErr.Message, "does not exist")
}

func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}

	return s[:limit] + "…"
}
