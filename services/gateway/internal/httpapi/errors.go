package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxRequestBodyBytes bounds every JSON request body this package's
// handlers decode (IssueCard, AdjustCardLimit, RationBridgeFunding,
// SimulateDeposit) — 1MB, the same cap and justification
// services/webhook-ingestion/internal/webhookhandler's handler already
// uses ("webhook bodies are small JSON"): every field any of these
// request types carries is a handful of int64/bool/short-string
// values, never anything that legitimately needs more than a few
// hundred bytes. Without this, decodeJSONBody had no bound at all —
// json.NewDecoder(r.Body) reads and buffers the entire body before
// ever validating a single field or checking the caller's credential,
// so one request with an arbitrarily large body (no auth required:
// decoding happens before clientContext's credential is even
// forwarded to the downstream gRPC call that would reject it) could
// balloon this process's memory by the full body size. Reproduced
// live before this fix: a single unauthenticated 300MB POST to
// /v1/cards drove gateway's RSS from ~20MB to ~1.3GB.
const maxRequestBodyBytes = 1 << 20

// errBodyTooLarge is returned by decodeJSONBody when r's body exceeds
// maxRequestBodyBytes, so call sites can map it to 413 instead of the
// 400 every other decode failure (malformed JSON, unknown field) gets.
var errBodyTooLarge = errors.New("request body exceeds the 1MB limit")

// errorBody is the JSON shape every non-2xx response in this package
// uses: {"error": {"code": "<grpc status code name>", "message": "..."}}.
// The code is always the underlying gRPC status code's name (e.g.
// "INVALID_ARGUMENT", "NOT_FOUND") — stable and language-neutral,
// unlike an HTTP status alone — so a REST caller can branch on it the
// same way a gRPC caller would branch on codes.Code without needing a
// second, REST-specific error taxonomy.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeJSON encodes v as the response body with the given status code.
// Encoding failures are logged, not returned — the response is already
// partially written by the time json.Marshal could fail on a
// pathological value, so there is nothing more useful to do than log.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Error("httpapi: failed encoding response body", "error", err)
	}
}

// writeBadRequest writes a 400 with a plain message — used for
// request-shape problems (malformed JSON, missing path parameter) this
// package catches before ever calling into the gRPC client, so there is
// no gRPC status to translate.
func writeBadRequest(w http.ResponseWriter, log *slog.Logger, message string) {
	body := errorBody{}
	body.Error.Code = codes.InvalidArgument.String()
	body.Error.Message = message
	writeJSON(w, log, http.StatusBadRequest, body)
}

// writeRequestEntityTooLarge writes a 413 for a decodeJSONBody call
// that failed because the body exceeded maxRequestBodyBytes — a
// distinct case from writeBadRequest's 400, since the request shape
// may otherwise have been perfectly valid.
func writeRequestEntityTooLarge(w http.ResponseWriter, log *slog.Logger, message string) {
	body := errorBody{}
	body.Error.Code = codes.ResourceExhausted.String()
	body.Error.Message = message
	writeJSON(w, log, http.StatusRequestEntityTooLarge, body)
}

// writeGRPCError translates err (expected to be, or wrap, a
// status.Status from a GatewayClient call) into this package's JSON
// error envelope and the HTTP status code that best matches the gRPC
// status code — not a second error taxonomy, just a mapping table, per
// this package's doc comment.
func writeGRPCError(w http.ResponseWriter, log *slog.Logger, err error) {
	st, ok := status.FromError(err)
	if !ok {
		// Not a status error at all (shouldn't happen for a gRPC client
		// call, but handled defensively) — surface it as an opaque 500
		// rather than guessing.
		log.Error("httpapi: non-gRPC error from GatewayClient call", "error", err)
		body := errorBody{}
		body.Error.Code = codes.Unknown.String()
		body.Error.Message = "internal error"
		writeJSON(w, log, http.StatusInternalServerError, body)
		return
	}

	body := errorBody{}
	body.Error.Code = st.Code().String()
	body.Error.Message = st.Message()
	writeJSON(w, log, httpStatusForCode(st.Code()), body)
}

// httpStatusForCode maps a gRPC status code to the HTTP status code a
// REST caller expects — the standard mapping grpc-gateway itself uses,
// restated here since this package hand-rolls the translation rather
// than generating it.
func httpStatusForCode(code codes.Code) int {
	switch code {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499 // non-standard, but conventional for "client canceled"
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// decodeJSONBody decodes r's body into dst, returning a caller-friendly
// error on malformed JSON (including an empty body, which
// json.Decoder.Decode would otherwise report as io.EOF) and capping
// the body at maxRequestBodyBytes via http.MaxBytesReader — see that
// constant's doc comment for why this cap exists. w is only used to
// let MaxBytesReader close the connection once the limit is exceeded
// (the same contract net/http itself documents); decodeJSONBody never
// writes a response to it. A caller whose body exceeds the limit gets
// errBodyTooLarge back (wrapped, so errors.Is still matches) — callers
// should check for it with errors.Is and respond 413 via
// writeRequestEntityTooLarge instead of the normal 400.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			return errBodyTooLarge
		}
		return errors.New("malformed JSON request body: " + err.Error())
	}
	return nil
}
