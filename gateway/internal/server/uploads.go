package server

import (
	"strings"
	"time"

	"kaiak/internal/provider"
)

// DefaultUploadMemory is the upload budget when KAIAK_UPLOAD_MEMORY_BYTES is unset: ten
// uploads at the default 25 MiB cap, in a pool apart from the body budget, so uploads
// cannot starve chat of body memory.
const DefaultUploadMemory int64 = 256 << 20

// DefaultUploadReadTimeout bounds an upload's arrival when KAIAK_UPLOAD_READ_TIMEOUT_MS
// is unset: 25 MiB within the body-read deadline's 60 s needs about 3.5 Mbit/s of
// uplink, which mobile clients often lack.
const DefaultUploadReadTimeout = 300 * time.Second

// Uploads are what an upload's body is read under besides the config's cap
// (docs/specs/GATEWAY.md, Request pipeline → Uploads).
type Uploads struct {
	// Budget is the upload budget: the upload bytes held at once, apart from the body
	// budget.
	Budget *BodyBudget
	// ReadTimeout bounds an upload's body arrival, in place of the listener's
	// body-read deadline.
	ReadTimeout time.Duration
}

// uploadFormat is an upload format's reading of its parts (docs/specs/GATEWAY.md,
// Client API → owned parts).
type uploadFormat struct {
	// lists are the names of the parts that may repeat — lists, such as image edits'
	// image[]; a repeat of any other name is refused, as a repeated JSON member is.
	lists []string
	// read reads the format's owned parts past model, from parts in the client's
	// order, and sets what the later stages read of them: the request's media and its
	// input estimate.
	read func(rq *request, parts []provider.FormPart) *apiError
}

// readUpload reads an upload endpoint's request (docs/specs/GATEWAY.md, Request
// pipeline → Uploads): its content type, before the body is read; the body, capped by
// the snapshot's max_upload_bytes and by the whole upload budget (a larger body could
// never be held), taken from the upload budget step by step and arriving within the
// upload read deadline; its parts (readForm), model among them, then the format's own.
// The body holds its share of the upload budget until it is dropped (releaseBody, also
// a finisher).
func readUpload(rq *request, uploads Uploads) *apiError {
	boundary, apiErr := uploadBoundary(rq.r.Header.Get("Content-Type"))
	if apiErr != nil {
		return apiErr
	}
	limit := min(rq.snapshot.MaxUploadBytes, uploads.Budget.Limit())
	if rq.r.ContentLength > limit {
		return errBodyTooLarge(limit)
	}
	if rq.r.ContentLength != 0 {
		// The listener set the body-read deadline as it took the request (Listen).
		setBodyDeadline(rq.w, uploads.ReadTimeout)
	}
	rq.finishers = append(rq.finishers, rq.releaseBody)
	body, apiErr := rq.readBody(uploads.Budget, limit)
	if apiErr != nil {
		return apiErr
	}
	clearBodyDeadline(rq.w)
	rq.body = body
	format := rq.endpoint.upload
	if rq.form, apiErr = readForm(body, boundary, format.lists); apiErr != nil {
		return apiErr
	}
	if apiErr = readModelPart(rq); apiErr != nil {
		return apiErr
	}
	rq.inbound.Sequences = 1
	return format.read(rq, rq.form)
}

// uploadText is part's value as an owned text part: a file part in its place is the
// wrong type, 400 invalid_type naming it (docs/specs/GATEWAY.md, Client API → errors).
func uploadText(part provider.FormPart) (string, *apiError) {
	if part.File {
		return "", errInvalidType(part.Name, "a text part")
	}
	return string(part.Body), nil
}

// readStreamPart holds an upload's stream part to the one rule of both upload formats
// (docs/specs/GATEWAY.md, Client API → owned parts): false, in any case — anything
// else is refused, 400 invalid_value. vLLM and vLLM-Omni read a form boolean in several
// spellings (true, 1, on, yes), and the gateway relays neither format's stream: an
// allowlist, so a spelling a server learns later is refused until it is judged.
func readStreamPart(part provider.FormPart) *apiError {
	value, apiErr := uploadText(part)
	if apiErr != nil {
		return apiErr
	}
	if !strings.EqualFold(value, "false") {
		return errStreamPart()
	}
	return nil
}

// readModelPart reads the model an upload names: its model part — a text part, one
// at most (readForm) — which must not be empty.
func readModelPart(rq *request) *apiError {
	for _, part := range rq.form {
		if part.Name != "model" {
			continue
		}
		if part.File {
			return errInvalidType("model", "a text part")
		}
		if len(part.Body) == 0 {
			return errMissingModel()
		}
		rq.model = string(part.Body)
		return nil
	}
	return errMissingModel()
}
