package web

import (
	"net/http"

	"github.com/axin2024/upgrade-agent/internal/records"
)

func (a App) estateRoutes(mux *http.ServeMux) {
	const collection = root + "/estate/repositories"
	mux.HandleFunc("GET "+collection, a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		items, err := a.Store.EstateRepositories(r.Context(), i, r.URL.Query().Get("after"))
		a.result(w, items, err)
	}))
	mux.HandleFunc("POST "+collection, a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.EstateRegistration
		if !a.read(w, r, &in, 4<<10) {
			return
		}
		result, err := a.Store.RegisterEstateRepository(r.Context(), i, r.Header.Get("Idempotency-Key"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("PUT "+collection+"/{repo}/policy", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.EstatePolicyInput
		if !a.read(w, r, &in, 32<<10) {
			return
		}
		result, err := a.Store.SetEstatePolicy(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("repo"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+collection+"/{repo}/snapshots", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		id := r.PathValue("repo")
		if err := a.Store.CheckEstateSnapshotUpload(r.Context(), i, id); err != nil {
			a.result(w, nil, err)
			return
		}
		var in records.EstateSnapshotInput
		if !a.read(w, r, &in, 32<<20) {
			return
		}
		result, err := a.Store.PinEstateSnapshot(r.Context(), i, r.Header.Get("Idempotency-Key"), id, in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("GET "+collection+"/{repo}/contracts", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.EstateCatalog(r.Context(), i, r.PathValue("repo"), r.URL.Query().Get("snapshot"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+root+"/estate/impact", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.EstateImpactInput
		if !a.read(w, r, &in, 32<<10) {
			return
		}
		result, err := a.Store.EstateImpact(r.Context(), i, in)
		a.result(w, result, err)
	}))
}
