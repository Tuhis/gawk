package readapi

// TH8's HTTP surface over the annotation store (docs/36 UD16).
//
// This is the ONE write path in the whole read API, and it is deliberately
// narrow: create, list, delete. Nothing here changes a measurement, and nothing
// here can reach a session file — the store writes to its own sibling
// directory, which `Store.Prune` does not walk.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/annotations"
)

// AnnotationStore is TH8's write path, as an interface so the API can be built
// without one — a deployment that has not enabled annotations answers 501
// rather than panicking, and a test can substitute a fake.
type AnnotationStore interface {
	Add(annotations.Annotation) (annotations.Annotation, error)
	Delete(id string) error
	List(annotations.Query) ([]annotations.Annotation, error)
}

// Annotations lists notes matching a query. Returns an empty slice (never nil)
// where no store is wired, so a chart's marker layer has nothing to special-case.
func (a *API) Annotations(q annotations.Query) ([]annotations.Annotation, error) {
	if a.annotations == nil {
		return []annotations.Annotation{}, nil
	}
	out, err := a.annotations.List(q)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []annotations.Annotation{}
	}
	return out, nil
}

func (a *API) handleAnnotationList(w http.ResponseWriter, r *http.Request) {
	q := annotations.Query{
		SessionID:    r.URL.Query().Get("session"),
		BroadcastKey: r.URL.Query().Get("broadcast"),
		FromMs:       int64Of(r, "from"),
		ToMs:         int64Of(r, "to"),
	}
	out, err := a.Annotations(q)
	writeJSON(w, out, err)
}

func (a *API) handleAnnotationCreate(w http.ResponseWriter, r *http.Request) {
	if a.annotations == nil {
		http.Error(w, "annotations are not enabled on this deployment", http.StatusNotImplemented)
		return
	}
	var in annotations.Annotation
	// Bounded: this is a public-shaped handler on a non-public listener, and an
	// unbounded body would still be an unbounded body.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "malformed annotation", http.StatusBadRequest)
		return
	}
	out, err := a.annotations.Add(in)
	if errors.Is(err, annotations.ErrInvalid) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, out, err)
}

func (a *API) handleAnnotationDelete(w http.ResponseWriter, r *http.Request) {
	if a.annotations == nil {
		http.Error(w, "annotations are not enabled on this deployment", http.StatusNotImplemented)
		return
	}
	err := a.annotations.Delete(r.PathValue("id"))
	switch {
	case errors.Is(err, annotations.ErrNotFound):
		http.Error(w, "no such annotation", http.StatusNotFound)
	case errors.Is(err, annotations.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// AnnotationsEnabled says whether the write path exists here, so the UI can hide
// an affordance that would only ever 501.
func (a *API) AnnotationsEnabled() bool { return a.annotations != nil }
