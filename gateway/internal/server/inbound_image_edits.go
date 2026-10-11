package server

import (
	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// imageEditsUpload is image edits' upload format (docs/specs/GATEWAY.md, Client API →
// owned parts: image edits): the input images may repeat — image and image[] file
// parts, url[] data URLs — and every other part is single-valued.
var imageEditsUpload = &uploadFormat{lists: []string{"image", "image[]", "url[]"}, read: readImageEditParts}

// readImageEditParts reads an image edit's owned parts past model (readUpload), in the
// client's order (docs/specs/GATEWAY.md, Client API → owned parts: image edits):
//   - n, the images the edit asks for (editImages), reserved against the image limits;
//   - response_format, b64_json only (absent means it): the images are counted from
//     the answer's data, which no other format carries countably;
//   - stream, false or absent (readStreamPart): vLLM-Omni streams an edit only on a
//     multi-stage pipeline, in events of its own, and the gateway relays none;
//   - lora, refused whatever its value: it names a server path the backend loads;
//   - url, url[], mask_image and reference_image given as text, refused unless they
//     are inline data — vLLM-Omni fetches an http… string; given as file parts they
//     pass;
//   - the input estimate: the prompt's text, plus each input image — an image or
//     image[] file part, never inspected, or a url or url[] data URL — as a media item.
//
// Everything else — size, negative_prompt, seed, OpenAI's mask (vLLM-Omni's is
// mask_image, and the gateway does not translate it), … — passes untouched, rebuilt
// as received.
func readImageEditParts(rq *request, parts []provider.FormPart) *apiError {
	images, prompt, inputs := int64(1), int64(0), int64(0)
	for _, part := range parts {
		var apiErr *apiError
		switch part.Name {
		case "n":
			images, apiErr = editImages(part, rq.snapshot.MaxImagesPerRequest)
		case "response_format":
			apiErr = readImageFormatPart(part)
		case "stream":
			apiErr = readStreamPart(part)
		case "lora":
			apiErr = errExternalReference("lora")
		case "url", "url[]", "mask_image", "reference_image":
			if part.File {
				continue
			}
			if !inlineData(string(part.Body)) {
				apiErr = errExternalReference(part.Name)
			} else if part.Name == "url" || part.Name == "url[]" {
				inputs++
			}
		case "image", "image[]":
			if part.File {
				inputs++
			}
		case "prompt":
			if !part.File {
				prompt = int64(len(part.Body))
			}
		}
		if apiErr != nil {
			return apiErr
		}
	}
	rq.inbound.Media = map[config.Measure]int64{config.MeasureImages: images}
	rq.input = accounting.EstimateUploadInput(prompt, inputs)
	return nil
}

// editImages is the images an edit's n part asks for (uploadWholeNumber) — below 1
// counted as 1, for the backend to refuse — refused above limit,
// max_images_per_request (vLLM-Omni bounds no edit's n).
func editImages(part provider.FormPart, limit int64) (int64, *apiError) {
	n, apiErr := uploadWholeNumber(part, "n")
	if apiErr != nil {
		return 0, apiErr
	}
	images := max(n, 1)
	if images > limit {
		return 0, errTooManyImages(images, limit)
	}
	return images, nil
}

// readImageFormatPart holds an edit's response_format part to b64_json, as on image
// generation (parseImagesFields).
func readImageFormatPart(part provider.FormPart) *apiError {
	value, apiErr := uploadText(part)
	if apiErr != nil {
		return apiErr
	}
	if value != "b64_json" {
		return errResponseFormatUnsupported("response_format", "b64_json")
	}
	return nil
}
