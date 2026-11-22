package api

import (
	"net/http"

	"github.com/Resinat/Resin/internal/service"
)

// HandleFreePoolStatus handles GET /api/v1/free-pool/status.
func HandleFreePoolStatus(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := cp.GetFreePoolStatus()
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, status)
	}
}

// HandlePatchFreePoolConfig handles PATCH /api/v1/free-pool/config.
func HandlePatchFreePoolConfig(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := readRawBodyOrWriteInvalid(w, r)
		if !ok {
			return
		}
		status, err := cp.UpdateFreePoolConfig(raw)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, status)
	}
}

// HandleFreePoolRun handles POST /api/v1/free-pool/actions/run.
func HandleFreePoolRun(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := cp.TriggerFreePoolRun(); err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
	}
}

// HandleFreePoolStop handles POST /api/v1/free-pool/actions/stop.
func HandleFreePoolStop(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cancelled, err := cp.CancelFreePoolRun()
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"cancelled": cancelled})
	}
}

// HandleFreePoolSubscription handles GET /api/v1/free-pool/subscription.
func HandleFreePoolSubscription(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := cp.GetFreePoolSubscriptionJSON()
		if err != nil {
			writeServiceError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}
