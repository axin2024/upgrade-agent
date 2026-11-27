package web

import (
	"errors"
	"net/http"

	"github.com/axin2024/upgrade-agent/internal/records"
)

const campaignCollection = root + "/migration-campaigns"
const campaignItem = campaignCollection + "/{campaign}"

func (a App) campaignRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+campaignCollection, a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.MigrationCampaigns(r.Context(), i, r.URL.Query().Get("after"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignCollection, a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.CampaignInput
		if !a.read(w, r, &in, 64<<10) {
			return
		}
		result, err := a.Store.CreateMigrationCampaign(r.Context(), i, r.Header.Get("Idempotency-Key"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignCollection+"/legacy", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.LegacyCampaignInput
		if !a.read(w, r, &in, 4<<10) {
			return
		}
		result, err := a.Store.ImportLegacyCampaign(r.Context(), i, r.Header.Get("Idempotency-Key"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("GET "+campaignItem, a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.MigrationCampaign(r.Context(), i, r.PathValue("campaign"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignItem+"/freeze", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in struct {
			Expected uint64 `json:"campaign_rev"`
		}
		if !a.read(w, r, &in, 2<<10) {
			return
		}
		result, err := a.Store.FreezeMigrationCampaign(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("campaign"), in.Expected)
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignItem+"/rebase", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.CampaignRebaseInput
		if !a.read(w, r, &in, 4<<10) {
			return
		}
		result, err := a.Store.RebaseCampaign(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("campaign"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignItem+"/candidates", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.CampaignCandidateInput
		if !a.read(w, r, &in, 64<<10) {
			return
		}
		result, err := a.Store.GenerateCampaignCandidate(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("campaign"), in, a.Model)
		if err == nil && result.State == "running" {
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusAccepted, result)
			return
		}
		a.result(w, result, err)
	}))
	mux.HandleFunc("GET "+campaignItem+"/candidates/{candidate}", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.CampaignCandidate(r.Context(), i, r.PathValue("campaign"), r.PathValue("candidate"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignItem+"/candidates/{candidate}/hunks/{hunk}", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.HunkDecision
		if !a.read(w, r, &in, 4<<10) {
			return
		}
		result, err := a.Store.ReviewCampaignHunk(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("campaign"),
			r.PathValue("candidate"), r.PathValue("hunk"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignItem+"/rehearsals", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.RehearsalInput
		if !a.read(w, r, &in, 32<<10) {
			return
		}
		campaign := r.PathValue("campaign")
		result, err := a.Store.SubmitCampaignRehearsal(r.Context(), i, r.Header.Get("Idempotency-Key"), campaign, in)
		if errors.Is(err, records.ErrInvalid) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "field": "repo_digests / contract_set / runner_profile"})
			return
		}
		if err != nil {
			a.result(w, nil, err)
			return
		}
		w.Header().Set("Location", campaignCollection+"/"+campaign+"/rehearsals/"+result.ID)
		writeJSON(w, http.StatusAccepted, result)
	}))
	mux.HandleFunc("GET "+campaignItem+"/rehearsals/{rehearsal}", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.CampaignRehearsal(r.Context(), i, r.PathValue("campaign"), r.PathValue("rehearsal"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignItem+"/rehearsals/{rehearsal}/retry", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.RetryCampaignRehearsal(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("campaign"), r.PathValue("rehearsal"))
		if err != nil {
			a.result(w, nil, err)
			return
		}
		writeJSON(w, http.StatusAccepted, result)
	}))
	mux.HandleFunc("POST "+campaignItem+"/bundles", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.CampaignExportInput
		if !a.read(w, r, &in, 4<<10) {
			return
		}
		result, err := a.Store.ExportCampaignBundle(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("campaign"), in)
		if err == nil && result.State == "drifted" {
			writeJSON(w, http.StatusConflict, result)
			return
		}
		a.result(w, result, err)
	}))
	mux.HandleFunc("GET "+campaignItem+"/bundles/{bundle}", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.CampaignBundle(r.Context(), i, r.PathValue("campaign"), r.PathValue("bundle"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+campaignItem+"/bundles/{bundle}/adopt", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.CampaignAdoptionInput
		if !a.read(w, r, &in, 4<<10) {
			return
		}
		result, err := a.Store.AdoptCampaignBundle(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("campaign"), r.PathValue("bundle"), in)
		if err == nil && result.State == "drifted" {
			writeJSON(w, http.StatusConflict, result)
			return
		}
		a.result(w, result, err)
	}))
}
