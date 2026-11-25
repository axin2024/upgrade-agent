// Package web exposes the scoped workspace API and browser workbench.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/model"
	"github.com/axin2024/upgrade-agent/internal/records"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

//go:embed ui
var assets embed.FS

type App struct {
	Store      *records.Store
	Validation validation.Backend
	Model      *model.Client
	Logger     *slog.Logger
}

type authenticated func(http.ResponseWriter, *http.Request, records.Identity)

const root = "/api/v1"

func (a App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Store.Ready(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"state": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"state": "ready"})
	})
	static, err := fs.Sub(assets, "ui")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(static)
	mux.Handle("GET /{$}", files)
	mux.Handle("GET /app.js", files)
	mux.Handle("GET /app.css", files)
	mux.HandleFunc("GET "+root+"/workspace", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		ws, err := a.Store.Current(r.Context(), i)
		a.result(w, struct {
			records.Identity
			records.Workspace
		}{i, ws}, err)
	}))
	mux.HandleFunc("GET "+root+"/history", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var after uint64
		if value := r.URL.Query().Get("after"); value != "" {
			var err error
			after, err = strconv.ParseUint(value, 10, 64)
			if err != nil {
				a.result(w, nil, records.ErrInvalid)
				return
			}
		}
		result, err := a.Store.History(r.Context(), i, after)
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+root+"/snapshots", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.BaselineInput
		if !a.read(w, r, &in, 32<<20) {
			return
		}
		result, err := a.Store.Pin(r.Context(), i, r.Header.Get("Idempotency-Key"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+root+"/plans", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.PlanInput
		if !a.read(w, r, &in, 2<<20) {
			return
		}
		result, err := a.Store.CreatePlan(r.Context(), i, r.Header.Get("Idempotency-Key"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("GET "+root+"/plans/{plan}", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.Plan(r.Context(), i, r.PathValue("plan"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+root+"/plans/{plan}/batches/{batch}/candidates", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.CandidateInput
		if !a.read(w, r, &in, 16<<10) {
			return
		}
		result, err := a.Store.Generate(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("plan"),
			r.PathValue("batch"), in, a.Validation, a.Model)
		a.job(w, result, err)
	}))
	mux.HandleFunc("GET "+root+"/candidates/{candidate}", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.Candidate(r.Context(), i, r.PathValue("candidate"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("GET "+root+"/candidates/{candidate}/patch", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.Patch(r.Context(), i, r.PathValue("candidate"))
		if err != nil {
			a.result(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="migration.patch"`)
		_, _ = io.WriteString(w, result)
	}))
	mux.HandleFunc("POST "+root+"/candidates/{candidate}/validations", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.VerifyInput
		if !a.read(w, r, &in, 16<<10) {
			return
		}
		result, err := a.Store.Verify(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("candidate"), in, a.Validation)
		a.job(w, result, err)
	}))
	mux.HandleFunc("GET "+root+"/validations/{report}", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.Report(r.Context(), i, r.PathValue("report"))
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+root+"/candidates/{candidate}/decisions", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in records.DecisionInput
		if !a.read(w, r, &in, 16<<10) {
			return
		}
		result, err := a.Store.Decide(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("candidate"), in)
		a.result(w, result, err)
	}))
	mux.HandleFunc("POST "+root+"/plans/{plan}/exports", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		var in struct {
			Baseline string `json:"baseline"`
		}
		if !a.read(w, r, &in, 4096) {
			return
		}
		result, err := a.Store.Export(r.Context(), i, r.Header.Get("Idempotency-Key"), r.PathValue("plan"), in.Baseline)
		a.result(w, result, err)
	}))
	mux.HandleFunc("GET "+root+"/jobs/{request}", a.auth(func(w http.ResponseWriter, r *http.Request, i records.Identity) {
		result, err := a.Store.Job(r.Context(), i, r.PathValue("request"))
		a.job(w, result, err)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (a App) auth(action authenticated) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer token required"})
			return
		}
		i, err := a.Store.Identify(r.Context(), secret)
		if errors.Is(err, records.ErrDenied) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer token expired or invalid"})
			return
		}
		if err != nil {
			a.result(w, nil, err)
			return
		}
		action(w, r, i)
	}
}

func (a App) read(w http.ResponseWriter, r *http.Request, destination any, size int64) bool {
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected application/json"})
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, size))
	dec.DisallowUnknownFields()
	var extra any
	if err := dec.Decode(destination); err != nil || !errors.Is(dec.Decode(&extra), io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or oversized JSON request"})
		return false
	}
	return true
}

func (a App) job(w http.ResponseWriter, job records.Job, err error) {
	if err == nil && job.State == "running" {
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusAccepted, job)
		return
	}
	a.result(w, job, err)
}

func (a App) result(w http.ResponseWriter, value any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, value)
		return
	}
	status, message := http.StatusInternalServerError, "workspace operation failed"
	switch {
	case errors.Is(err, records.ErrMissing):
		status, message = http.StatusNotFound, err.Error()
	case errors.Is(err, records.ErrDenied):
		status, message = http.StatusForbidden, err.Error()
	case errors.Is(err, records.ErrInvalid):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, records.ErrConflict):
		status, message = http.StatusConflict, err.Error()
	default:
		if a.Logger != nil {
			a.Logger.Error("workspace operation failed", "error_type", fmt.Sprintf("%T", err))
		}
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
